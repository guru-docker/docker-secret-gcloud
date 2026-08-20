package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"net/http"

	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"github.com/docker/go-plugins-helpers/secrets"
	"github.com/rs/zerolog/log"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// createPath is an extension to the docker.secretprovider protocol, not part of
// it. Docker only ever calls /SecretProvider.GetSecret, so nothing reaches this
// route except a client speaking to the plugin socket directly -- which means
// root on a node where the plugin is installed.
//
// It exists so a secret can be provisioned using the credentials the plugin
// already holds, rather than requiring GCP credentials wherever an operator
// happens to be sitting.
const createPath = "/SecretProvider.CreateSecret"

// generateMax bounds Generate. Secret Manager caps a payload at 64KiB; this is
// far below that, because a request for megabytes of randomness is a mistake
// rather than an intent.
const generateMax = 1024

// CreateRequest is the body of a createPath call.
type CreateRequest struct {
	// SecretName and SecretLabels are resolved exactly as they are for Get, so
	// a secret is created under the same name the driver will later read.
	SecretName   string            `json:",omitempty"`
	SecretLabels map[string]string `json:",omitempty"`

	// Exactly one of Value and Generate must be set. Value travels over the
	// plugin socket and never enters the swarm's raft store, which is what
	// makes supplying it here safe in a way that a label would not be.
	Value    []byte `json:",omitempty"`
	Generate int    `json:",omitempty"`

	// IfMissing tolerates a secret that already exists, adding a new version to
	// it instead of failing. Without it, creating twice is an error, so a typo
	// cannot silently add a version to the wrong secret.
	IfMissing bool `json:",omitempty"`
}

// CreateResponse reports what was done. The value is deliberately not echoed
// back, including when it was generated: it belongs in Secret Manager, and
// anything that needs it can read it through the Get route.
type CreateResponse struct {
	Resource string `json:",omitempty"`
	Version  string `json:",omitempty"`
	Created  bool   `json:",omitempty"`
	Err      string `json:",omitempty"`
}

// registerCreate adds the route to the handler the secrets helper built. The
// helper embeds sdk.Handler, whose mux is shared, so this lands on the same
// socket as GetSecret.
func registerCreate(h *secrets.Handler, d *DockerDriver) {
	h.HandleFunc(createPath, func(w http.ResponseWriter, r *http.Request) {
		var req CreateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeCreateResponse(w, CreateResponse{Err: "malformed request: " + err.Error()})
			return
		}
		writeCreateResponse(w, d.Create(req))
	})
}

func writeCreateResponse(w http.ResponseWriter, resp CreateResponse) {
	w.Header().Set("Content-Type", "application/json")
	if resp.Err != "" {
		w.WriteHeader(http.StatusInternalServerError)
	}
	_ = json.NewEncoder(w).Encode(resp)
}

// Create provisions a secret and adds a version holding the value.
func (d *DockerDriver) Create(req CreateRequest) CreateResponse {
	log.Info().Any("method", "create").
		Str("secret", req.SecretName).
		Msgf("%v", req.SecretLabels)

	value, err := payloadFor(req)
	if err != nil {
		return CreateResponse{Err: err.Error()}
	}

	// Resolving through the Get path keeps one definition of where a named
	// secret lives; the version segment is discarded, since a new secret starts
	// at version 1 regardless.
	ref, err := resolve(secrets.Request{SecretName: req.SecretName, SecretLabels: req.SecretLabels}, d.project)
	if err != nil {
		return CreateResponse{Err: err.Error()}
	}

	parent, secretID, err := splitResource(ref.Resource)
	if err != nil {
		return CreateResponse{Err: err.Error()}
	}

	ctx, cancel := context.WithTimeout(context.Background(), d.timeout)
	defer cancel()

	client, err := d.clientFor(ctx)
	if err != nil {
		return CreateResponse{Err: err.Error()}
	}

	created, err := createSecret(ctx, client, parent, secretID, req.IfMissing)
	if err != nil {
		return CreateResponse{Err: err.Error()}
	}

	version, err := client.AddSecretVersion(ctx, &secretmanagerpb.AddSecretVersionRequest{
		Parent: parent + "/secrets/" + secretID,
		Payload: &secretmanagerpb.SecretPayload{
			Data:       value,
			DataCrc32C: crc32cOf(value),
		},
	})
	if err != nil {
		return CreateResponse{Err: logError("failed to add a version to %s/secrets/%s: %v", parent, secretID, err).Error()}
	}

	log.Info().Any("method", "create").
		Str("secret", req.SecretName).
		Msgf("wrote %d bytes to %s (new secret: %v)", len(value), version.GetName(), created)

	return CreateResponse{
		Resource: parent + "/secrets/" + secretID,
		Version:  version.GetName(),
		Created:  created,
	}
}

// createSecret creates the container for the versions. It reports whether the
// secret was new; an existing one is only tolerated when the caller said so.
func createSecret(ctx context.Context, client versionAccessor, parent, secretID string, ifMissing bool) (bool, error) {
	secret := &secretmanagerpb.Secret{}
	// A regional secret takes its location from the parent and must not carry a
	// replication policy; a global one requires it.
	if !isRegional(parent) {
		secret.Replication = &secretmanagerpb.Replication{
			Replication: &secretmanagerpb.Replication_Automatic_{
				Automatic: &secretmanagerpb.Replication_Automatic{},
			},
		}
	}

	_, err := client.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{
		Parent:   parent,
		SecretId: secretID,
		Secret:   secret,
	})
	if err == nil {
		return true, nil
	}

	if status.Code(err) == codes.AlreadyExists {
		if !ifMissing {
			return false, logError("secret %s/secrets/%s already exists; pass IfMissing to add a version to it", parent, secretID)
		}
		return false, nil
	}

	return false, logError("failed to create %s/secrets/%s: %v", parent, secretID, err)
}

// payloadFor returns the bytes to store, generating them when asked.
func payloadFor(req CreateRequest) ([]byte, error) {
	switch {
	case len(req.Value) > 0 && req.Generate > 0:
		return nil, logError("Value and Generate are mutually exclusive")
	case len(req.Value) > 0:
		return req.Value, nil
	case req.Generate > 0:
		if req.Generate > generateMax {
			return nil, logError("Generate is %d bytes, more than the %d this plugin will produce", req.Generate, generateMax)
		}
		value := make([]byte, req.Generate)
		if _, err := rand.Read(value); err != nil {
			return nil, logError("failed to generate a value: %v", err)
		}
		return value, nil
	default:
		return nil, logError("no value: set Value, or Generate to a byte count")
	}
}

func isRegional(parent string) bool {
	return regionalParentPattern.MatchString(parent)
}
