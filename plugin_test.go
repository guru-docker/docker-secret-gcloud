package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"github.com/docker/go-plugins-helpers/secrets"
	"github.com/googleapis/gax-go/v2"
	"github.com/rs/zerolog"
)

func TestMain(m *testing.M) {
	// The driver logs every request; keep test output readable.
	zerolog.SetGlobalLevel(zerolog.Disabled)
	os.Exit(m.Run())
}

// fakeAccessor stands in for *secretmanager.Client so no test reaches Google.
type fakeAccessor struct {
	mu sync.Mutex

	requests []string
	closed   int

	payload   []byte
	crc       *int64
	noPayload bool
	err       error

	created   []*secretmanagerpb.CreateSecretRequest
	added     []*secretmanagerpb.AddSecretVersionRequest
	createErr error
	addErr    error
}

func (f *fakeAccessor) AccessSecretVersion(_ context.Context, req *secretmanagerpb.AccessSecretVersionRequest, _ ...gax.CallOption) (*secretmanagerpb.AccessSecretVersionResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.requests = append(f.requests, req.GetName())
	if f.err != nil {
		return nil, f.err
	}
	if f.noPayload {
		return &secretmanagerpb.AccessSecretVersionResponse{Name: req.GetName()}, nil
	}

	return &secretmanagerpb.AccessSecretVersionResponse{
		Name:    req.GetName(),
		Payload: &secretmanagerpb.SecretPayload{Data: f.payload, DataCrc32C: f.crc},
	}, nil
}

func (f *fakeAccessor) CreateSecret(_ context.Context, req *secretmanagerpb.CreateSecretRequest, _ ...gax.CallOption) (*secretmanagerpb.Secret, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.created = append(f.created, req)
	if f.createErr != nil {
		return nil, f.createErr
	}
	return &secretmanagerpb.Secret{Name: req.GetParent() + "/secrets/" + req.GetSecretId()}, nil
}

func (f *fakeAccessor) AddSecretVersion(_ context.Context, req *secretmanagerpb.AddSecretVersionRequest, _ ...gax.CallOption) (*secretmanagerpb.SecretVersion, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.added = append(f.added, req)
	if f.addErr != nil {
		return nil, f.addErr
	}
	return &secretmanagerpb.SecretVersion{Name: req.GetParent() + "/versions/1"}, nil
}

func (f *fakeAccessor) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.closed++
	return nil
}

func (f *fakeAccessor) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]string(nil), f.requests...)
}

// newTestDriver returns a driver wired to fake, with no default project unless
// one is given.
func newTestDriver(t *testing.T, fake *fakeAccessor, project string) *DockerDriver {
	t.Helper()

	d := newDockerDriver(project, time.Second)
	d.newClient = func(context.Context) (versionAccessor, error) { return fake, nil }

	return d
}

// --- request resolution ----------------------------------------------------

func TestResolve(t *testing.T) {
	tests := []struct {
		name           string
		req            secrets.Request
		defaultProject string
		want           string
	}{
		{
			name:           "secret name and plugin project",
			req:            secrets.Request{SecretName: "api-key"},
			defaultProject: "acme-prod",
			want:           "projects/acme-prod/secrets/api-key/versions/latest",
		},
		{
			name: "project label overrides the plugin default",
			req: secrets.Request{
				SecretName:   "api-key",
				SecretLabels: map[string]string{labelProject: "acme-dev"},
			},
			defaultProject: "acme-prod",
			want:           "projects/acme-dev/secrets/api-key/versions/latest",
		},
		{
			name: "secret label overrides the docker name",
			req: secrets.Request{
				SecretName:   "app_api_key_v3",
				SecretLabels: map[string]string{labelSecret: "api-key"},
			},
			defaultProject: "acme-prod",
			want:           "projects/acme-prod/secrets/api-key/versions/latest",
		},
		{
			name: "pinned version",
			req: secrets.Request{
				SecretName:   "api-key",
				SecretLabels: map[string]string{labelVersion: "7"},
			},
			defaultProject: "acme-prod",
			want:           "projects/acme-prod/secrets/api-key/versions/7",
		},
		{
			name: "full resource wins over everything",
			req: secrets.Request{
				SecretName: "api-key",
				SecretLabels: map[string]string{
					labelResource: "projects/other/secrets/elsewhere/versions/2",
					labelProject:  "acme-dev",
					labelSecret:   "ignored",
					labelVersion:  "9",
				},
			},
			defaultProject: "acme-prod",
			want:           "projects/other/secrets/elsewhere/versions/2",
		},
		{
			name: "regional resource",
			req: secrets.Request{
				SecretLabels: map[string]string{labelResource: "projects/acme/locations/europe-west1/secrets/api-key/versions/3"},
			},
			want: "projects/acme/locations/europe-west1/secrets/api-key/versions/3",
		},
		{
			name: "resource without a version reads the latest",
			req: secrets.Request{
				SecretLabels: map[string]string{labelResource: "projects/acme/secrets/api-key"},
			},
			want: "projects/acme/secrets/api-key/versions/latest",
		},
		{
			name: "surrounding whitespace is ignored",
			req: secrets.Request{
				SecretName:   "api-key",
				SecretLabels: map[string]string{labelProject: "  acme-prod  "},
			},
			want: "projects/acme-prod/secrets/api-key/versions/latest",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ref, err := resolve(tt.req, tt.defaultProject)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if ref.Resource != tt.want {
				t.Errorf("Resource = %q, want %q", ref.Resource, tt.want)
			}
		})
	}
}

func TestResolve_Errors(t *testing.T) {
	tests := []struct {
		name           string
		req            secrets.Request
		defaultProject string
		wantErr        string
	}{
		{
			name:    "no project anywhere",
			req:     secrets.Request{SecretName: "api-key"},
			wantErr: labelProject,
		},
		{
			name:           "no secret name anywhere",
			req:            secrets.Request{},
			defaultProject: "acme-prod",
			wantErr:        labelSecret,
		},
		{
			name:           "docker name is not a valid secret manager id",
			req:            secrets.Request{SecretName: "api key/v3"},
			defaultProject: "acme-prod",
			wantErr:        "valid Secret Manager id",
		},
		{
			name: "malformed resource",
			req: secrets.Request{
				SecretLabels: map[string]string{labelResource: "acme/api-key"},
			},
			wantErr: "not a Secret Manager resource",
		},
		{
			name: "resource with a trailing segment",
			req: secrets.Request{
				SecretLabels: map[string]string{labelResource: "projects/acme/secrets/api-key/versions/1/extra"},
			},
			wantErr: "not a Secret Manager resource",
		},
		{
			name: "unparseable do_not_reuse",
			req: secrets.Request{
				SecretName:   "api-key",
				SecretLabels: map[string]string{labelDoNotReuse: "sometimes"},
			},
			defaultProject: "acme-prod",
			wantErr:        "not a boolean",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := resolve(tt.req, tt.defaultProject)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not mention %q", err, tt.wantErr)
			}
		})
	}
}

func TestResolve_DoNotReuse(t *testing.T) {
	tests := []struct {
		label string
		want  bool
	}{
		{"", false},
		{"true", true},
		{"1", true},
		{"false", false},
	}

	for _, tt := range tests {
		t.Run("label="+tt.label, func(t *testing.T) {
			ref, err := resolve(secrets.Request{
				SecretName:   "api-key",
				SecretLabels: map[string]string{labelDoNotReuse: tt.label},
			}, "acme-prod")
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if ref.DoNotReuse != tt.want {
				t.Errorf("DoNotReuse = %v, want %v", ref.DoNotReuse, tt.want)
			}
		})
	}
}

// --- Get -------------------------------------------------------------------

func TestGet_ReturnsThePayload(t *testing.T) {
	want := []byte("s3cr3t")
	fake := &fakeAccessor{payload: want, crc: crc32cOf(want)}
	d := newTestDriver(t, fake, "acme-prod")

	resp := d.Get(secrets.Request{SecretName: "api-key"})

	if resp.Err != "" {
		t.Fatalf("Get: %s", resp.Err)
	}
	if string(resp.Value) != string(want) {
		t.Errorf("Value = %q, want %q", resp.Value, want)
	}
	if got := fake.seen(); len(got) != 1 || got[0] != "projects/acme-prod/secrets/api-key/versions/latest" {
		t.Errorf("requested %v", got)
	}
}

func TestGet_PassesDoNotReuseThrough(t *testing.T) {
	fake := &fakeAccessor{payload: []byte("s3cr3t")}
	d := newTestDriver(t, fake, "acme-prod")

	resp := d.Get(secrets.Request{
		SecretName:   "api-key",
		SecretLabels: map[string]string{labelDoNotReuse: "true"},
	})

	if resp.Err != "" {
		t.Fatalf("Get: %s", resp.Err)
	}
	if !resp.DoNotReuse {
		t.Error("DoNotReuse = false, want true")
	}
}

func TestGet_ResolutionFailureDoesNotCallTheAPI(t *testing.T) {
	fake := &fakeAccessor{payload: []byte("s3cr3t")}
	d := newTestDriver(t, fake, "")

	resp := d.Get(secrets.Request{SecretName: "api-key"})

	if resp.Err == "" {
		t.Fatal("expected an error when no project is configured")
	}
	if got := fake.seen(); len(got) != 0 {
		t.Errorf("API was called anyway: %v", got)
	}
}

func TestGet_ReportsAPIFailure(t *testing.T) {
	fake := &fakeAccessor{err: errors.New("permission denied")}
	d := newTestDriver(t, fake, "acme-prod")

	resp := d.Get(secrets.Request{SecretName: "api-key"})

	if resp.Err == "" {
		t.Fatal("expected the API failure to surface")
	}
	if !strings.Contains(resp.Err, "permission denied") {
		t.Errorf("Err = %q, want the underlying cause", resp.Err)
	}
	if len(resp.Value) != 0 {
		t.Errorf("a failed Get must not return a value, got %q", resp.Value)
	}
}

func TestGet_RejectsAnEmptyPayload(t *testing.T) {
	fake := &fakeAccessor{noPayload: true}
	d := newTestDriver(t, fake, "acme-prod")

	if resp := d.Get(secrets.Request{SecretName: "api-key"}); resp.Err == "" {
		t.Fatal("expected an error for a response with no payload")
	}
}

// A corrupted payload must not be handed to a container as if it were the
// secret; Secret Manager ships a checksum precisely so this is detectable.
func TestGet_RejectsACorruptedPayload(t *testing.T) {
	wrong := int64(1)
	fake := &fakeAccessor{payload: []byte("s3cr3t"), crc: &wrong}
	d := newTestDriver(t, fake, "acme-prod")

	resp := d.Get(secrets.Request{SecretName: "api-key"})

	if resp.Err == "" {
		t.Fatal("expected a checksum failure")
	}
	if !strings.Contains(resp.Err, "crc32c") {
		t.Errorf("Err = %q, want a checksum message", resp.Err)
	}
}

func TestGet_ReusesTheClient(t *testing.T) {
	fake := &fakeAccessor{payload: []byte("s3cr3t")}
	d := newDockerDriver("acme-prod", time.Second)

	var built int
	d.newClient = func(context.Context) (versionAccessor, error) {
		built++
		return fake, nil
	}

	for i := 0; i < 3; i++ {
		if resp := d.Get(secrets.Request{SecretName: "api-key"}); resp.Err != "" {
			t.Fatalf("Get %d: %s", i, resp.Err)
		}
	}

	if built != 1 {
		t.Errorf("client built %d times, want 1", built)
	}
}

// A plugin installed before its credentials are in place must recover once they
// appear, rather than caching the failure for its lifetime.
func TestGet_RetriesAFailedClientBuild(t *testing.T) {
	fake := &fakeAccessor{payload: []byte("s3cr3t")}
	d := newDockerDriver("acme-prod", time.Second)

	var attempts int
	d.newClient = func(context.Context) (versionAccessor, error) {
		attempts++
		if attempts == 1 {
			return nil, errors.New("no credentials")
		}
		return fake, nil
	}

	if resp := d.Get(secrets.Request{SecretName: "api-key"}); resp.Err == "" {
		t.Fatal("expected the first request to fail")
	}
	if resp := d.Get(secrets.Request{SecretName: "api-key"}); resp.Err != "" {
		t.Fatalf("second request: %s", resp.Err)
	}
	if attempts != 2 {
		t.Errorf("newClient called %d times, want 2", attempts)
	}
}

func TestGet_HonoursTheTimeout(t *testing.T) {
	d := newDockerDriver("acme-prod", time.Millisecond)

	var deadline time.Time
	d.newClient = func(ctx context.Context) (versionAccessor, error) {
		deadline, _ = ctx.Deadline()
		return &fakeAccessor{payload: []byte("s3cr3t")}, nil
	}

	if resp := d.Get(secrets.Request{SecretName: "api-key"}); resp.Err != "" {
		t.Fatalf("Get: %s", resp.Err)
	}
	if deadline.IsZero() {
		t.Fatal("the API call was made without a deadline")
	}
}

func TestClose(t *testing.T) {
	fake := &fakeAccessor{payload: []byte("s3cr3t")}
	d := newTestDriver(t, fake, "acme-prod")

	if err := d.Close(); err != nil {
		t.Fatalf("Close before any request: %v", err)
	}
	if fake.closed != 0 {
		t.Error("closed a client that was never built")
	}

	if resp := d.Get(secrets.Request{SecretName: "api-key"}); resp.Err != "" {
		t.Fatal(resp.Err)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if fake.closed != 1 {
		t.Errorf("client closed %d times, want 1", fake.closed)
	}
}

// --- checksum --------------------------------------------------------------

func TestVerifyCRC32C(t *testing.T) {
	data := []byte("s3cr3t")

	if err := verifyCRC32C(data, nil); err != nil {
		t.Errorf("an absent checksum must be accepted: %v", err)
	}
	if err := verifyCRC32C(data, crc32cOf(data)); err != nil {
		t.Errorf("a matching checksum must be accepted: %v", err)
	}

	wrong := int64(42)
	if err := verifyCRC32C(data, &wrong); err == nil {
		t.Error("a mismatched checksum must be rejected")
	}
}

// --- environment -----------------------------------------------------------

func TestTimeoutFromEnv(t *testing.T) {
	tests := []struct {
		value   string
		want    time.Duration
		wantErr bool
	}{
		{"", defaultTimeout, false},
		{"5s", 5 * time.Second, false},
		{"1m30s", 90 * time.Second, false},
		{"soon", 0, true},
		{"0", 0, true},
		{"-5s", 0, true},
	}

	for _, tt := range tests {
		t.Run("GCLOUD_TIMEOUT="+tt.value, func(t *testing.T) {
			t.Setenv("GCLOUD_TIMEOUT", tt.value)

			got, err := timeoutFromEnv()
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("timeoutFromEnv: %v", err)
			}
			if got != tt.want {
				t.Errorf("timeout = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestLogLevel(t *testing.T) {
	tests := []struct {
		value string
		want  zerolog.Level
	}{
		{"", zerolog.InfoLevel},
		{"0", zerolog.InfoLevel},
		{"1", zerolog.DebugLevel},
		{"true", zerolog.DebugLevel},
		{"nonsense", zerolog.InfoLevel},
	}

	for _, tt := range tests {
		t.Run("DEBUG="+tt.value, func(t *testing.T) {
			t.Setenv("DEBUG", tt.value)

			if got := logLevel(); got != tt.want {
				t.Errorf("logLevel() = %s, want %s", got, tt.want)
			}
		})
	}
}

// --- credentials -----------------------------------------------------------

func TestClientOptions(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "credentials.json")

	original := credentialsFile
	t.Cleanup(func() { credentialsFile = original })

	t.Run("nothing configured falls back to ADC", func(t *testing.T) {
		t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")
		credentialsFile = filepath.Join(dir, "absent.json")
		if opts := clientOptions(); len(opts) != 0 {
			t.Errorf("got %d options, want none", len(opts))
		}
	})

	t.Run("a mounted credentials file is used", func(t *testing.T) {
		t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")
		if err := os.WriteFile(key, []byte(`{"type":"service_account"}`), 0600); err != nil {
			t.Fatal(err)
		}
		credentialsFile = key

		if opts := clientOptions(); len(opts) != 1 {
			t.Errorf("got %d options, want 1", len(opts))
		}
	})

	t.Run("GOOGLE_APPLICATION_CREDENTIALS wins", func(t *testing.T) {
		t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", key)
		credentialsFile = key

		if opts := clientOptions(); len(opts) != 0 {
			t.Errorf("got %d options, want none: ADC reads the variable itself", len(opts))
		}
	})
}

// --- the startup credentials check -----------------------------------------

func TestCheckCredentials(t *testing.T) {
	dir := t.TempDir()
	present := filepath.Join(dir, "credentials.json")
	if err := os.WriteFile(present, []byte(`{"type":"service_account"}`), 0600); err != nil {
		t.Fatal(err)
	}
	absent := filepath.Join(dir, "absent.json")

	original := credentialsFile
	t.Cleanup(func() { credentialsFile = original })

	tests := []struct {
		name     string
		envPath  string
		mounted  string
		required string
		wantErr  string
	}{
		{
			name:    "a mounted credentials file is accepted",
			mounted: present,
		},
		{
			name:    "GOOGLE_APPLICATION_CREDENTIALS is accepted",
			envPath: present,
			mounted: absent,
		},
		{
			name:    "GOOGLE_APPLICATION_CREDENTIALS pointing at nothing is fatal",
			envPath: absent,
			mounted: present,
			wantErr: "cannot read",
		},
		{
			name:    "an empty mount falls back to ADC by default",
			mounted: absent,
		},
		{
			name:     "an empty mount is fatal when the file is required",
			mounted:  absent,
			required: "1",
			wantErr:  "not mounted when the plugin was enabled",
		},
		{
			name:     "a required file that is present is accepted",
			mounted:  present,
			required: "true",
		},
		{
			name:     "an unparseable requirement is fatal",
			mounted:  absent,
			required: "maybe",
			wantErr:  "not a boolean",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", tt.envPath)
			t.Setenv("GCLOUD_REQUIRE_CREDENTIALS_FILE", tt.required)
			credentialsFile = tt.mounted

			err := checkCredentials()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("checkCredentials: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected the plugin to refuse to start")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not mention %q", err, tt.wantErr)
			}
		})
	}
}
