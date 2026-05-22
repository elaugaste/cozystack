package backupcontroller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newFakeClient(objs ...client.Object) client.Client {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

func defaultCfg() BackupCredentialsConfig {
	return BackupCredentialsConfig{
		SourceNamespace:  "cozy-backup-controller",
		SourceSecretName: "bucket-cozy-backups-system-credentials",
		TargetSecretName: "cozy-backups-creds",
		Endpoint:         "http://seaweedfs-s3.tenant-root.svc:8333",
		Region:           "us-east-1",
	}
}

func flatSourceSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "cozy-backup-controller",
			Name:      "bucket-cozy-backups-system-credentials",
		},
		Data: map[string][]byte{
			"accessKey":  []byte("AK"),
			"secretKey":  []byte("SK"),
			"endpoint":   []byte("seaweedfs-s3.tenant-root.svc:8333"),
			"bucketName": []byte("cozy-backups"),
		},
	}
}

// TestIsEnabled covers each field-empty case driving the configured/
// disabled split — strategies that need cozy-backups-creds rely on this
// gate so a partial config does not silently project anything.
func TestIsEnabled(t *testing.T) {
	cases := []struct {
		name string
		cfg  BackupCredentialsConfig
		want bool
	}{
		{"all empty", BackupCredentialsConfig{}, false},
		{"source ns missing", BackupCredentialsConfig{SourceSecretName: "s", TargetSecretName: "t"}, false},
		{"source name missing", BackupCredentialsConfig{SourceNamespace: "ns", TargetSecretName: "t"}, false},
		{"target missing", BackupCredentialsConfig{SourceNamespace: "ns", SourceSecretName: "s"}, false},
		{"complete", BackupCredentialsConfig{SourceNamespace: "ns", SourceSecretName: "s", TargetSecretName: "t"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.IsEnabled(); got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

// TestProject_HappyPath_FlatKeys covers the human-friendly Secret format
// rendered by packages/system/bucket/templates/user-credentials.yaml.
// Asserts the projected Secret carries every key the supported drivers
// consume.
func TestProject_HappyPath_FlatKeys(t *testing.T) {
	src := flatSourceSecret()
	c := newFakeClient(src)
	ctx := context.Background()

	if err := ProjectBackupCredentials(ctx, c, defaultCfg(), "tenant-acme"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := &corev1.Secret{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "tenant-acme", Name: "cozy-backups-creds"}, got); err != nil {
		t.Fatalf("projected Secret not found: %v", err)
	}
	if got.Labels[managedByLabel] != managedByValue {
		t.Fatalf("managed-by label missing or wrong: %q", got.Labels[managedByLabel])
	}
	for k, want := range map[string][]byte{
		"AWS_ACCESS_KEY_ID":     []byte("AK"),
		"AWS_SECRET_ACCESS_KEY": []byte("SK"),
		"accessKey":             []byte("AK"),
		"secretKey":             []byte("SK"),
		"endpoint":              []byte("seaweedfs-s3.tenant-root.svc:8333"),
		"bucketName":            []byte("cozy-backups"),
		"region":                []byte("us-east-1"),
	} {
		if string(got.Data[k]) != string(want) {
			t.Errorf("key %s: got %q want %q", k, got.Data[k], want)
		}
	}
	if cloud := string(got.Data["cloud"]); cloud != "[default]\naws_access_key_id=AK\naws_secret_access_key=SK\n" {
		t.Errorf("cloud key malformed: %q", cloud)
	}
	var blob struct {
		Accounts map[string]struct {
			APIKey string `json:"api_key"`
			Secret string `json:"secret"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(got.Data["blob_credentials.json"], &blob); err != nil {
		t.Fatalf("blob_credentials.json invalid: %v", err)
	}
	host := "seaweedfs-s3.tenant-root.svc:8333"
	if blob.Accounts[host].APIKey != "AK" || blob.Accounts[host].Secret != "SK" {
		t.Fatalf("blob host record wrong: %+v", blob.Accounts)
	}
}

// TestProject_HappyPath_BucketInfo covers the raw COSI Secret format
// (single BucketInfo JSON blob) — the fallback that lets the cluster
// keep working when the user-credentials renderer has not run yet, or
// when an admin wires an external S3 Secret manually.
func TestProject_HappyPath_BucketInfo(t *testing.T) {
	bucketInfo := []byte(`{
	  "spec": {
	    "bucketName": "external-backups",
	    "secretS3": {
	      "accessKeyID": "ROOT",
	      "accessSecretKey": "SHHH",
	      "endpoint": "https://s3.example.com",
	      "region": "eu-west-1"
	    }
	  }
	}`)
	src := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "cozy-backup-controller",
			Name:      "bucket-cozy-backups-system-credentials",
		},
		Data: map[string][]byte{"BucketInfo": bucketInfo},
	}
	c := newFakeClient(src)
	ctx := context.Background()

	if err := ProjectBackupCredentials(ctx, c, defaultCfg(), "tenant-acme"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := &corev1.Secret{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "tenant-acme", Name: "cozy-backups-creds"}, got); err != nil {
		t.Fatalf("projected Secret not found: %v", err)
	}
	if string(got.Data["accessKey"]) != "ROOT" || string(got.Data["secretKey"]) != "SHHH" {
		t.Fatalf("bucketInfo flat keys not projected: %s/%s", got.Data["accessKey"], got.Data["secretKey"])
	}
	if string(got.Data["endpoint"]) != "s3.example.com" {
		t.Errorf("endpoint scheme not stripped: %s", got.Data["endpoint"])
	}
	if string(got.Data["region"]) != "eu-west-1" {
		t.Errorf("region from BucketInfo lost: %s", got.Data["region"])
	}
	if string(got.Data["bucketName"]) != "external-backups" {
		t.Errorf("bucketName from BucketInfo lost: %s", got.Data["bucketName"])
	}
}

// TestProject_SourceMissing surfaces a transient ProjectionError so
// reconcilers can requeue rather than terminally fail BackupJobs while
// the bucket controller catches up.
func TestProject_SourceMissing(t *testing.T) {
	c := newFakeClient()
	err := ProjectBackupCredentials(context.Background(), c, defaultCfg(), "tenant-acme")
	var perr *ProjectionError
	if !errors.As(err, &perr) {
		t.Fatalf("expected ProjectionError, got %T: %v", err, err)
	}
	if perr.Reason != ReasonSourceMissing {
		t.Fatalf("expected reason %s, got %s", ReasonSourceMissing, perr.Reason)
	}
	got := &corev1.Secret{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "tenant-acme", Name: "cozy-backups-creds"}, got); err == nil {
		t.Fatalf("target should remain absent on transient failure")
	}
}

// TestProject_SourceMalformed_NoKeys exercises a Secret that exists but
// carries neither flat keys nor a BucketInfo blob. Terminal — reconcilers
// must surface the operator misconfiguration rather than retry forever.
func TestProject_SourceMalformed_NoKeys(t *testing.T) {
	src := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "cozy-backup-controller",
			Name:      "bucket-cozy-backups-system-credentials",
		},
		Data: map[string][]byte{"endpoint": []byte("x")},
	}
	c := newFakeClient(src)
	err := ProjectBackupCredentials(context.Background(), c, defaultCfg(), "tenant-acme")
	var perr *ProjectionError
	if !errors.As(err, &perr) {
		t.Fatalf("expected ProjectionError, got %T: %v", err, err)
	}
	if perr.Reason != ReasonSourceMalformed {
		t.Fatalf("expected reason %s, got %s", ReasonSourceMalformed, perr.Reason)
	}
}

// TestProject_RefusesUnownedTarget is the core ownership guard:
// CreateOrUpdate must not stomp on a Secret that some other actor wrote
// (manually-created, restored from snapshot, etc.). Drives review fix #3.
func TestProject_RefusesUnownedTarget(t *testing.T) {
	src := flatSourceSecret()
	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "tenant-acme",
			Name:      "cozy-backups-creds",
			// No managed-by label.
		},
		Data: map[string][]byte{"some-key": []byte("user-data")},
	}
	c := newFakeClient(src, existing)

	err := ProjectBackupCredentials(context.Background(), c, defaultCfg(), "tenant-acme")
	var perr *ProjectionError
	if !errors.As(err, &perr) {
		t.Fatalf("expected ProjectionError, got %T: %v", err, err)
	}
	if perr.Reason != ReasonTargetNotOwned {
		t.Fatalf("expected reason %s, got %s", ReasonTargetNotOwned, perr.Reason)
	}
	// Verify the existing Secret has not been mutated.
	got := &corev1.Secret{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "tenant-acme", Name: "cozy-backups-creds"}, got); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got.Data["some-key"]) != "user-data" {
		t.Fatalf("existing Secret was overwritten: %v", got.Data)
	}
	if _, leaked := got.Data["AWS_ACCESS_KEY_ID"]; leaked {
		t.Fatalf("projector leaked credentials into unowned Secret")
	}
}

// TestProject_RewritesOwnedTarget confirms idempotency: a Secret that
// already carries the managed-by label is updated in place, not refused.
func TestProject_RewritesOwnedTarget(t *testing.T) {
	src := flatSourceSecret()
	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "tenant-acme",
			Name:      "cozy-backups-creds",
			Labels:    map[string]string{managedByLabel: managedByValue},
		},
		Data: map[string][]byte{"stale": []byte("old")},
	}
	c := newFakeClient(src, existing)

	if err := ProjectBackupCredentials(context.Background(), c, defaultCfg(), "tenant-acme"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := &corev1.Secret{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "tenant-acme", Name: "cozy-backups-creds"}, got); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got.Data["AWS_ACCESS_KEY_ID"]) != "AK" {
		t.Fatalf("owned target not refreshed: %v", got.Data)
	}
}

// TestBuildFDBBlobCredentials_EndpointForms locks in stripScheme behaviour
// for both URL-form and bare host:port endpoints, plus the empty-endpoint
// guard that prevents writing a JSON document keyed by "".
func TestBuildFDBBlobCredentials_EndpointForms(t *testing.T) {
	for _, tc := range []struct {
		name     string
		endpoint string
		wantHost string
		wantErr  bool
	}{
		{"url with scheme", "http://s3.example.com:8333", "s3.example.com:8333", false},
		{"https url", "https://s3.example.com", "s3.example.com", false},
		{"bare host:port", "s3.example.com:8333", "s3.example.com:8333", false},
		{"empty", "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, err := buildFDBBlobCredentials(tc.endpoint, "AK", "SK")
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error for empty endpoint")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			var doc struct {
				Accounts map[string]struct {
					APIKey string `json:"api_key"`
				} `json:"accounts"`
			}
			if err := json.Unmarshal(data, &doc); err != nil {
				t.Fatalf("invalid JSON: %v", err)
			}
			if _, ok := doc.Accounts[tc.wantHost]; !ok {
				t.Fatalf("account %q not in %+v", tc.wantHost, doc.Accounts)
			}
		})
	}
}

// TestProject_Disabled is a sanity check: with an empty config, the
// projector must be a no-op (used by Phase-1 clusters still on the
// legacy chart-managed flow).
func TestProject_Disabled(t *testing.T) {
	c := newFakeClient()
	if err := ProjectBackupCredentials(context.Background(), c, BackupCredentialsConfig{}, "tenant-acme"); err != nil {
		t.Fatalf("disabled projector must not error: %v", err)
	}
	got := &corev1.Secret{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "tenant-acme", Name: "cozy-backups-creds"}, got); err == nil {
		t.Fatalf("disabled projector wrote a Secret")
	}
}

// TestIsTransient pins the transient-vs-terminal classification used by
// both reconcilers' handleProjectionError implementations. A regression
// here (e.g. moving ReasonAPIError into the terminal bucket) would make
// every apiserver hiccup terminally fail tenant BackupJobs.
func TestIsTransient(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{&ProjectionError{Reason: ReasonSourceMissing}, true},
		{&ProjectionError{Reason: ReasonAPIError}, true},
		{&ProjectionError{Reason: ReasonSourceMalformed}, false},
		{&ProjectionError{Reason: ReasonTargetNotOwned}, false},
		{nil, false},
		{fmt.Errorf("plain error"), false},
	}
	for _, tc := range cases {
		got := IsTransient(tc.err)
		if got != tc.want {
			t.Errorf("IsTransient(%v): got %v want %v", tc.err, got, tc.want)
		}
	}
}

// TestProject_PartialFlatKeys_FallsBackToBucketInfoFields covers review
// finding #8: when the flat-key Secret carries access/secret but not
// bucket/endpoint/region (the user-credentials renderer doesn't emit
// region today), the BucketInfo fallback fills the gaps. Before the fix
// the JSON was only consulted when accessKey/secretKey were absent,
// silently dropping the operator-provided coordinates.
func TestProject_PartialFlatKeys_FallsBackToBucketInfoFields(t *testing.T) {
	src := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "cozy-backup-controller",
			Name:      "bucket-cozy-backups-system-credentials",
		},
		Data: map[string][]byte{
			"accessKey": []byte("AK"),
			"secretKey": []byte("SK"),
			"BucketInfo": []byte(`{
			  "spec": {
			    "bucketName": "from-bucketinfo",
			    "secretS3": {
			      "accessKeyID": "ignored",
			      "accessSecretKey": "ignored",
			      "endpoint": "https://nested.example.com",
			      "region": "eu-central-1"
			    }
			  }
			}`),
		},
	}
	c := newFakeClient(src)
	cfg := defaultCfg()
	cfg.Endpoint = "" // ensure fallback to BucketInfo, not config
	cfg.Region = ""
	if err := ProjectBackupCredentials(context.Background(), c, cfg, "tenant-acme"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := &corev1.Secret{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "tenant-acme", Name: "cozy-backups-creds"}, got); err != nil {
		t.Fatalf("projected Secret not found: %v", err)
	}
	// Flat keys win: the JSON access/secret were ignored.
	if string(got.Data["accessKey"]) != "AK" || string(got.Data["secretKey"]) != "SK" {
		t.Errorf("flat keys overridden by JSON: %q/%q", got.Data["accessKey"], got.Data["secretKey"])
	}
	// Missing flat fields backfilled from BucketInfo.
	if string(got.Data["endpoint"]) != "nested.example.com" {
		t.Errorf("endpoint not backfilled from BucketInfo (scheme stripped): %q", got.Data["endpoint"])
	}
	if string(got.Data["region"]) != "eu-central-1" {
		t.Errorf("region not backfilled from BucketInfo: %q", got.Data["region"])
	}
	if string(got.Data["bucketName"]) != "from-bucketinfo" {
		t.Errorf("bucketName not backfilled from BucketInfo: %q", got.Data["bucketName"])
	}
}

// TestProject_HappyPath_HTTPEndpoint covers review finding #9: the
// bucket chart now strips both http:// and https:// from the endpoint
// it materialises. Test the projector's stripScheme path explicitly for
// an http://-prefixed value coming through the flat-key path so a future
// regression in either layer fails loudly.
func TestProject_HappyPath_HTTPEndpoint(t *testing.T) {
	src := flatSourceSecret()
	src.Data["endpoint"] = []byte("http://seaweedfs-s3.tenant-root.svc:8333")
	c := newFakeClient(src)
	if err := ProjectBackupCredentials(context.Background(), c, defaultCfg(), "tenant-acme"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := &corev1.Secret{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "tenant-acme", Name: "cozy-backups-creds"}, got); err != nil {
		t.Fatalf("projected Secret not found: %v", err)
	}
	if string(got.Data["endpoint"]) != "seaweedfs-s3.tenant-root.svc:8333" {
		t.Errorf("http scheme not stripped: %q", got.Data["endpoint"])
	}
	// blob_credentials.json account key must also be the bare host.
	var blob struct {
		Accounts map[string]any `json:"accounts"`
	}
	if err := json.Unmarshal(got.Data["blob_credentials.json"], &blob); err != nil {
		t.Fatalf("invalid blob_credentials.json: %v", err)
	}
	if _, ok := blob.Accounts["seaweedfs-s3.tenant-root.svc:8333"]; !ok {
		t.Errorf("blob account host not stripped: %+v", blob.Accounts)
	}
}
