package main

import (
	"context"
	"fmt"
	"hash/crc32"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"github.com/docker/go-plugins-helpers/secrets"
	"github.com/googleapis/gax-go/v2"
	"github.com/rs/zerolog/log"
)

// Labels set on the swarm secret select which Secret Manager version to read.
const (
	labelResource   = "gcloud.resource"
	labelProject    = "gcloud.project"
	labelSecret     = "gcloud.secret"
	labelVersion    = "gcloud.version"
	labelDoNotReuse = "gcloud.do_not_reuse"
)

// Secret Manager ids are restricted to this alphabet. Docker secret names are
// not, so a name borrowed as the id is checked before it reaches the API.
var secretNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// A version resource, optionally regional, with the version segment optional so
// that "projects/p/secrets/s" can be completed to the latest version.
var resourcePattern = regexp.MustCompile(`^projects/[^/]+(?:/locations/[^/]+)?/secrets/[^/]+(/versions/[^/]+)?$`)

// versionAccessor is the part of *secretmanager.Client the driver uses; tests
// substitute a fake so no call reaches Google.
type versionAccessor interface {
	AccessSecretVersion(context.Context, *secretmanagerpb.AccessSecretVersionRequest, ...gax.CallOption) (*secretmanagerpb.AccessSecretVersionResponse, error)
	Close() error
}

// DockerDriver serves docker.secretprovider requests from Secret Manager.
type DockerDriver struct {
	sync.Mutex

	// project is the fallback used when a secret carries no project label.
	project string
	timeout time.Duration

	client versionAccessor
	// newClient builds the API client on first use; tests replace it. It is a
	// field rather than a call in the constructor so that a plugin installed
	// without working credentials still starts and reports the failure per
	// request instead of crash-looping.
	newClient func(context.Context) (versionAccessor, error)
}

func newDockerDriver(project string, timeout time.Duration) *DockerDriver {
	log.Info().Any("method", "new driver").Msgf("project=%q timeout=%s", project, timeout)

	return &DockerDriver{
		project:   project,
		timeout:   timeout,
		newClient: newSecretManagerClient,
	}
}

// secretRef is a resolved request: exactly which version to read, and whether
// the value may be shared between tasks.
type secretRef struct {
	Resource   string
	DoNotReuse bool
}

// Get implements secrets.Driver.
func (d *DockerDriver) Get(req secrets.Request) secrets.Response {
	log.Info().Any("method", "get").
		Str("secret", req.SecretName).
		Str("service", req.ServiceName).
		Str("task", req.TaskID).
		Msgf("%v", req.SecretLabels)

	ref, err := resolve(req, d.project)
	if err != nil {
		return secrets.Response{Err: err.Error()}
	}

	ctx, cancel := context.WithTimeout(context.Background(), d.timeout)
	defer cancel()

	value, err := d.access(ctx, ref.Resource)
	if err != nil {
		return secrets.Response{Err: err.Error()}
	}

	log.Info().Any("method", "get").
		Str("secret", req.SecretName).
		Msgf("delivered %d bytes from %s", len(value), ref.Resource)

	return secrets.Response{Value: value, DoNotReuse: ref.DoNotReuse}
}

// Close releases the API client. Safe to call on a driver that never built one.
func (d *DockerDriver) Close() error {
	d.Lock()
	defer d.Unlock()

	if d.client == nil {
		return nil
	}

	err := d.client.Close()
	d.client = nil
	return err
}

// access reads one secret version and checks it arrived intact.
func (d *DockerDriver) access(ctx context.Context, resource string) ([]byte, error) {
	client, err := d.clientFor(ctx)
	if err != nil {
		return nil, err
	}

	resp, err := client.AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{Name: resource})
	if err != nil {
		return nil, logError("failed to access %s: %v", resource, err)
	}
	if resp.GetPayload() == nil {
		return nil, logError("%s returned no payload", resource)
	}

	data := resp.GetPayload().GetData()
	if err = verifyCRC32C(data, resp.GetPayload().DataCrc32C); err != nil {
		return nil, logError("%s: %v", resource, err)
	}

	return data, nil
}

// clientFor returns the cached API client, building it on first use. A failed
// build is not cached, so a plugin that started before its credentials were in
// place recovers on the next request.
func (d *DockerDriver) clientFor(ctx context.Context) (versionAccessor, error) {
	d.Lock()
	defer d.Unlock()

	if d.client != nil {
		return d.client, nil
	}

	client, err := d.newClient(ctx)
	if err != nil {
		return nil, err
	}
	d.client = client

	return client, nil
}

// resolve turns a driver request into the version resource to read. Labels win
// over the request, so a docker secret can carry any name it likes.
func resolve(req secrets.Request, defaultProject string) (secretRef, error) {
	labels := req.SecretLabels

	doNotReuse, err := boolLabel(labels, labelDoNotReuse)
	if err != nil {
		return secretRef{}, err
	}

	if resource := strings.TrimSpace(labels[labelResource]); resource != "" {
		resource, err = normalizeResource(resource)
		if err != nil {
			return secretRef{}, err
		}
		return secretRef{Resource: resource, DoNotReuse: doNotReuse}, nil
	}

	project := firstNonEmpty(labels[labelProject], defaultProject)
	if project == "" {
		return secretRef{}, logError("no project: set the %q label on the secret, or GOOGLE_CLOUD_PROJECT on the plugin", labelProject)
	}

	name := firstNonEmpty(labels[labelSecret], req.SecretName)
	if name == "" {
		return secretRef{}, logError("no secret name in the request and no %q label", labelSecret)
	}
	if !secretNamePattern.MatchString(name) {
		return secretRef{}, logError("%q is not a valid Secret Manager id (letters, digits, '-' and '_'); name the secret with the %q label", name, labelSecret)
	}

	version := firstNonEmpty(labels[labelVersion], "latest")

	return secretRef{
		Resource:   fmt.Sprintf("projects/%s/secrets/%s/versions/%s", project, name, version),
		DoNotReuse: doNotReuse,
	}, nil
}

// normalizeResource validates a caller-supplied resource name and completes a
// secret-level one to its latest version.
func normalizeResource(resource string) (string, error) {
	m := resourcePattern.FindStringSubmatch(resource)
	if m == nil {
		return "", logError("%q is not a Secret Manager resource (projects/<project>/secrets/<secret>[/versions/<version>])", resource)
	}

	if m[1] == "" {
		resource += "/versions/latest"
	}

	return resource, nil
}

// verifyCRC32C checks the payload against the checksum Secret Manager stores
// with it. The checksum is optional in the response; absent means "not stored".
func verifyCRC32C(data []byte, want *int64) error {
	if want == nil {
		return nil
	}

	got := crc32.Checksum(data, crc32.MakeTable(crc32.Castagnoli))
	if uint32(*want) != got {
		return fmt.Errorf("payload failed its crc32c check (want %d, got %d)", uint32(*want), got)
	}

	return nil
}

func boolLabel(labels map[string]string, key string) (bool, error) {
	raw := strings.TrimSpace(labels[key])
	if raw == "" {
		return false, nil
	}

	value, err := strconv.ParseBool(raw)
	if err != nil {
		return false, logError("label %s=%q is not a boolean", key, raw)
	}

	return value, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}

func logError(format string, args ...interface{}) error {
	log.Error().Any("method", "logError").Msgf(format, args...)
	return fmt.Errorf(format, args...)
}
