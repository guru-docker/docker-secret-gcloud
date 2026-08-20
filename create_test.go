package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// --- payload selection ------------------------------------------------------

func TestPayloadFor(t *testing.T) {
	t.Run("an explicit value is used verbatim", func(t *testing.T) {
		got, err := payloadFor(CreateRequest{Value: []byte("hunter2")})
		if err != nil {
			t.Fatalf("payloadFor: %v", err)
		}
		if string(got) != "hunter2" {
			t.Errorf("value = %q", got)
		}
	})

	t.Run("generate produces the requested length", func(t *testing.T) {
		got, err := payloadFor(CreateRequest{Generate: 32})
		if err != nil {
			t.Fatalf("payloadFor: %v", err)
		}
		if len(got) != 32 {
			t.Errorf("generated %d bytes, want 32", len(got))
		}
	})

	// Two calls returning the same bytes would mean the source is not random.
	t.Run("generated values differ", func(t *testing.T) {
		a, _ := payloadFor(CreateRequest{Generate: 32})
		b, _ := payloadFor(CreateRequest{Generate: 32})
		if bytes.Equal(a, b) {
			t.Error("two generated values were identical")
		}
	})

	tests := []struct {
		name    string
		req     CreateRequest
		wantErr string
	}{
		{"neither", CreateRequest{}, "no value"},
		{"both", CreateRequest{Value: []byte("x"), Generate: 8}, "mutually exclusive"},
		{"absurd length", CreateRequest{Generate: generateMax + 1}, "more than the"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := payloadFor(tt.req)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not mention %q", err, tt.wantErr)
			}
		})
	}
}

// --- resource splitting -----------------------------------------------------

func TestSplitResource(t *testing.T) {
	tests := []struct {
		resource   string
		wantParent string
		wantID     string
	}{
		{"projects/acme/secrets/api-key/versions/latest", "projects/acme", "api-key"},
		{"projects/acme/secrets/api-key/versions/7", "projects/acme", "api-key"},
		{
			"projects/acme/locations/europe-west1/secrets/api-key/versions/latest",
			"projects/acme/locations/europe-west1", "api-key",
		},
	}

	for _, tt := range tests {
		t.Run(tt.resource, func(t *testing.T) {
			parent, id, err := splitResource(tt.resource)
			if err != nil {
				t.Fatalf("splitResource: %v", err)
			}
			if parent != tt.wantParent || id != tt.wantID {
				t.Errorf("got (%q, %q), want (%q, %q)", parent, id, tt.wantParent, tt.wantID)
			}
		})
	}

	if _, _, err := splitResource("not/a/resource"); err == nil {
		t.Error("expected an error for a malformed resource")
	}
}

// --- Create -----------------------------------------------------------------

func TestCreate_CreatesTheSecretAndAddsAVersion(t *testing.T) {
	fake := &fakeAccessor{}
	d := newTestDriver(t, fake, "acme-prod")

	resp := d.Create(CreateRequest{SecretName: "api-key", Value: []byte("hunter2")})

	if resp.Err != "" {
		t.Fatalf("Create: %s", resp.Err)
	}
	if !resp.Created {
		t.Error("Created = false, want true for a new secret")
	}
	if resp.Resource != "projects/acme-prod/secrets/api-key" {
		t.Errorf("Resource = %q", resp.Resource)
	}

	if len(fake.created) != 1 {
		t.Fatalf("CreateSecret called %d times, want 1", len(fake.created))
	}
	if got := fake.created[0].GetParent(); got != "projects/acme-prod" {
		t.Errorf("parent = %q", got)
	}
	if got := fake.created[0].GetSecretId(); got != "api-key" {
		t.Errorf("secret id = %q", got)
	}
	if fake.created[0].GetSecret().GetReplication().GetAutomatic() == nil {
		t.Error("a global secret must be created with automatic replication")
	}

	if len(fake.added) != 1 {
		t.Fatalf("AddSecretVersion called %d times, want 1", len(fake.added))
	}
	if got := string(fake.added[0].GetPayload().GetData()); got != "hunter2" {
		t.Errorf("payload = %q", got)
	}
	if fake.added[0].GetPayload().DataCrc32C == nil {
		t.Error("the payload was written without a checksum")
	}
}

// A regional secret takes its location from the parent, and the API rejects a
// replication policy on one.
func TestCreate_RegionalSecretHasNoReplicationPolicy(t *testing.T) {
	fake := &fakeAccessor{}
	d := newTestDriver(t, fake, "")

	resp := d.Create(CreateRequest{
		SecretName:   "api-key",
		SecretLabels: map[string]string{labelResource: "projects/acme/locations/europe-west1/secrets/api-key"},
		Value:        []byte("hunter2"),
	})

	if resp.Err != "" {
		t.Fatalf("Create: %s", resp.Err)
	}
	if got := fake.created[0].GetParent(); got != "projects/acme/locations/europe-west1" {
		t.Errorf("parent = %q", got)
	}
	if fake.created[0].GetSecret().GetReplication() != nil {
		t.Error("a regional secret must not carry a replication policy")
	}
}

func TestCreate_ExistingSecretIsAnErrorByDefault(t *testing.T) {
	fake := &fakeAccessor{createErr: status.Error(codes.AlreadyExists, "already exists")}
	d := newTestDriver(t, fake, "acme-prod")

	resp := d.Create(CreateRequest{SecretName: "api-key", Value: []byte("hunter2")})

	if resp.Err == "" {
		t.Fatal("expected creating an existing secret to fail")
	}
	if !strings.Contains(resp.Err, "IfMissing") {
		t.Errorf("Err = %q, want it to name the flag that allows this", resp.Err)
	}
	if len(fake.added) != 0 {
		t.Error("a version was added despite the failure")
	}
}

func TestCreate_IfMissingAddsAVersionToAnExistingSecret(t *testing.T) {
	fake := &fakeAccessor{createErr: status.Error(codes.AlreadyExists, "already exists")}
	d := newTestDriver(t, fake, "acme-prod")

	resp := d.Create(CreateRequest{SecretName: "api-key", Value: []byte("rotated"), IfMissing: true})

	if resp.Err != "" {
		t.Fatalf("Create: %s", resp.Err)
	}
	if resp.Created {
		t.Error("Created = true, want false when the secret already existed")
	}
	if len(fake.added) != 1 {
		t.Fatalf("AddSecretVersion called %d times, want 1", len(fake.added))
	}
	if got := string(fake.added[0].GetPayload().GetData()); got != "rotated" {
		t.Errorf("payload = %q", got)
	}
}

// A create failure that is not AlreadyExists must surface, not be swallowed.
func TestCreate_ReportsOtherAPIFailures(t *testing.T) {
	fake := &fakeAccessor{createErr: status.Error(codes.PermissionDenied, "no secrets.create")}
	d := newTestDriver(t, fake, "acme-prod")

	resp := d.Create(CreateRequest{SecretName: "api-key", Value: []byte("hunter2"), IfMissing: true})

	if resp.Err == "" {
		t.Fatal("expected a permission failure to surface")
	}
	if !strings.Contains(resp.Err, "no secrets.create") {
		t.Errorf("Err = %q, want the underlying cause", resp.Err)
	}
	if len(fake.added) != 0 {
		t.Error("a version was added after the secret failed to create")
	}
}

func TestCreate_ReportsAddVersionFailure(t *testing.T) {
	fake := &fakeAccessor{addErr: errors.New("quota exceeded")}
	d := newTestDriver(t, fake, "acme-prod")

	resp := d.Create(CreateRequest{SecretName: "api-key", Value: []byte("hunter2")})

	if resp.Err == "" || !strings.Contains(resp.Err, "quota exceeded") {
		t.Errorf("Err = %q, want the underlying cause", resp.Err)
	}
}

func TestCreate_ResolutionFailureDoesNotCallTheAPI(t *testing.T) {
	fake := &fakeAccessor{}
	d := newTestDriver(t, fake, "")

	resp := d.Create(CreateRequest{SecretName: "api-key", Value: []byte("hunter2")})

	if resp.Err == "" {
		t.Fatal("expected an error when no project is configured")
	}
	if len(fake.created) != 0 || len(fake.added) != 0 {
		t.Error("the API was called despite unresolvable input")
	}
}

func TestCreate_LabelsSelectTheTarget(t *testing.T) {
	fake := &fakeAccessor{}
	d := newTestDriver(t, fake, "acme-prod")

	resp := d.Create(CreateRequest{
		SecretName:   "docker-name",
		SecretLabels: map[string]string{labelProject: "acme-dev", labelSecret: "api-key"},
		Value:        []byte("hunter2"),
	})

	if resp.Err != "" {
		t.Fatalf("Create: %s", resp.Err)
	}
	if got := fake.created[0].GetParent(); got != "projects/acme-dev" {
		t.Errorf("parent = %q, want the label's project", got)
	}
	if got := fake.created[0].GetSecretId(); got != "api-key" {
		t.Errorf("secret id = %q, want the label's name", got)
	}
}

// The generated value must not come back in the response: it belongs in Secret
// Manager, and echoing it would put it in whatever logged the call.
func TestCreate_DoesNotEchoTheValue(t *testing.T) {
	fake := &fakeAccessor{}
	d := newTestDriver(t, fake, "acme-prod")

	resp := d.Create(CreateRequest{SecretName: "api-key", Generate: 32})
	if resp.Err != "" {
		t.Fatalf("Create: %s", resp.Err)
	}

	encoded, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	stored := fake.added[0].GetPayload().GetData()
	if bytes.Contains(encoded, stored) {
		t.Error("the response carried the secret value")
	}
}

// --- the HTTP route ---------------------------------------------------------

func TestCreateRoute(t *testing.T) {
	fake := &fakeAccessor{}
	d := newTestDriver(t, fake, "acme-prod")

	mux := http.NewServeMux()
	mux.HandleFunc(createPath, func(w http.ResponseWriter, r *http.Request) {
		var req CreateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeCreateResponse(w, CreateResponse{Err: "malformed request: " + err.Error()})
			return
		}
		writeCreateResponse(w, d.Create(req))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	t.Run("a well formed request creates the secret", func(t *testing.T) {
		body := `{"SecretName":"api-key","Value":"aHVudGVyMg=="}`
		res, err := http.Post(srv.URL+createPath, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()

		var resp CreateResponse
		if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
			t.Fatal(err)
		}
		if resp.Err != "" {
			t.Fatalf("route returned %s", resp.Err)
		}
		if res.StatusCode != http.StatusOK {
			t.Errorf("status = %d, want 200", res.StatusCode)
		}
		// "aHVudGVyMg==" is base64 for hunter2, which is how Go renders []byte.
		if got := string(fake.added[0].GetPayload().GetData()); got != "hunter2" {
			t.Errorf("payload = %q", got)
		}
	})

	t.Run("malformed JSON is rejected, not panicked on", func(t *testing.T) {
		res, err := http.Post(srv.URL+createPath, "application/json", strings.NewReader("{not json"))
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()

		var resp CreateResponse
		if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
			t.Fatal(err)
		}
		if resp.Err == "" {
			t.Error("expected an error for malformed JSON")
		}
		if res.StatusCode != http.StatusInternalServerError {
			t.Errorf("status = %d, want 500", res.StatusCode)
		}
	})
}

func TestCreate_HonoursTheTimeout(t *testing.T) {
	d := newDockerDriver("acme-prod", time.Millisecond)

	var deadline time.Time
	d.newClient = func(ctx context.Context) (versionAccessor, error) {
		deadline, _ = ctx.Deadline()
		return &fakeAccessor{}, nil
	}

	if resp := d.Create(CreateRequest{SecretName: "api-key", Generate: 16}); resp.Err != "" {
		t.Fatalf("Create: %s", resp.Err)
	}
	if deadline.IsZero() {
		t.Fatal("the API call was made without a deadline")
	}
}
