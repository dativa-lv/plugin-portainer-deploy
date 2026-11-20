// SPDX-License-Identifier: Apache-2.0

package plugin

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

const (
	existingImage = "app:1.0"
	missingImage  = "app:9.9.9"
)

var registryCredentials = &authn.Basic{Username: "ci-user", Password: "ci-password"}

type testRegistry struct {
	host                string
	receivedCredentials atomic.Bool
}

func (r *testRegistry) image(nameAndTag string) string {
	return r.host + "/" + nameAndTag
}

func startRegistry(t *testing.T, private bool) *testRegistry {
	t.Helper()

	reg := &testRegistry{}
	store := registry.New()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			reg.receivedCredentials.Store(true)
		}

		user, password, ok := r.BasicAuth()
		loggedIn := ok && user == registryCredentials.Username && password == registryCredentials.Password
		if private && !loggedIn {
			w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
			w.WriteHeader(http.StatusUnauthorized)

			return
		}

		store.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	reg.host = strings.TrimPrefix(server.URL, "http://")

	const layerBytes, layerCount = 64, 1
	img, err := random.Image(layerBytes, layerCount)
	if err != nil {
		t.Fatal(err)
	}

	ref, err := name.ParseReference(reg.image(existingImage))
	if err != nil {
		t.Fatal(err)
	}

	if err := remote.Write(ref, img, remote.WithAuth(registryCredentials)); err != nil {
		t.Fatal(err)
	}

	reg.receivedCredentials.Store(false)

	return reg
}

func TestImageChecker(t *testing.T) {
	privateRegistry := startRegistry(t, true)
	publicRegistry := startRegistry(t, false)

	checker := imageChecker{
		hosts: []string{privateRegistry.host},
		cred:  registryCredentials,
	}

	t.Run("existing images pass", func(t *testing.T) {
		images := []string{
			privateRegistry.image(existingImage),
			publicRegistry.image(existingImage),
			"${REGISTRY}/" + existingImage,
			"$REGISTRY/" + existingImage,
		}

		if err := checker.check(t.Context(), images); err != nil {
			t.Fatalf("want no error, got %v", err)
		}
	})

	t.Run("credentials are sent only to registry-hosts", func(t *testing.T) {
		publicRegistry.receivedCredentials.Store(false)

		if err := checker.check(t.Context(), []string{publicRegistry.image(existingImage)}); err != nil {
			t.Fatal(err)
		}

		if publicRegistry.receivedCredentials.Load() {
			t.Error("credentials were sent to a registry outside registry-hosts")
		}
	})

	t.Run("missing image fails and only it is named", func(t *testing.T) {
		missing := privateRegistry.image(missingImage)
		existing := publicRegistry.image(existingImage)

		err := checker.check(t.Context(), []string{missing, existing})
		if err == nil || !strings.Contains(err.Error(), missing) || strings.Contains(err.Error(), existing) {
			t.Errorf("want error naming only %s, got %v", missing, err)
		}
	})

	t.Run("private registry outside registry-hosts fails", func(t *testing.T) {
		withoutHosts := imageChecker{cred: registryCredentials}

		if err := withoutHosts.check(t.Context(), []string{privateRegistry.image(existingImage)}); err == nil {
			t.Error("want error: the registry was queried without credentials")
		}
	})
}

const apiStack = `
services:
  api:
    image: example.com/team/api:1.0
  migrations:
    image: example.com/team/api:1.0
`

const webStack = `
services:
  web:
    image: example.com/team/web:2.0
`

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()

	for path, content := range files {
		path = filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestStackImages(t *testing.T) {
	t.Run("glob returns each image once, sorted", func(t *testing.T) {
		dir := t.TempDir()
		writeFiles(t, dir, map[string]string{
			"dev/api.yaml": apiStack,
			"tv/web.yaml":  webStack,
		})

		images, err := stackImages(filepath.Join(dir, "*", "*.yaml"))
		if err != nil {
			t.Fatal(err)
		}

		want := "example.com/team/api:1.0,example.com/team/web:2.0"
		if got := strings.Join(images, ","); got != want {
			t.Errorf("got %s, want %s", got, want)
		}
	})

	t.Run("empty stack-path scans the whole workspace", func(t *testing.T) {
		dir := t.TempDir()
		writeFiles(t, dir, map[string]string{
			"prod/eu/api.yml":     apiStack,
			".woodpecker/ci.yaml": "services:\n  - name: db\n    image: example.com/ci/db:1\n",
			"app-config.yaml":     "services:\n  - name: other\n",
		})
		t.Chdir(dir)

		images, err := stackImages("")
		if err != nil {
			t.Fatal(err)
		}

		want := "example.com/team/api:1.0"
		if got := strings.Join(images, ","); got != want {
			t.Errorf("got %s, want %s", got, want)
		}
	})

	t.Run("invalid YAML fails and names the file", func(t *testing.T) {
		dir := t.TempDir()
		writeFiles(t, dir, map[string]string{
			"dev/broken.yaml": "services:\n  web:\n   image: x\n  - bad indent\n",
		})

		_, err := stackImages(filepath.Join(dir, "*", "*.yaml"))
		if err == nil || !strings.Contains(err.Error(), "broken.yaml") {
			t.Errorf("want error naming broken.yaml, got %v", err)
		}
	})

	t.Run("duplicate key fails and names the file", func(t *testing.T) {
		dir := t.TempDir()
		writeFiles(t, dir, map[string]string{
			"dev/dup.yaml": "services:\n  web:\n    image: x\n    image: y\n",
		})

		_, err := stackImages(filepath.Join(dir, "*", "*.yaml"))
		if err == nil || !strings.Contains(err.Error(), "dup.yaml") || !strings.Contains(err.Error(), "already defined") {
			t.Errorf("want duplicate key error naming dup.yaml, got %v", err)
		}
	})

	t.Run("undeclared alias fails", func(t *testing.T) {
		dir := t.TempDir()
		writeFiles(t, dir, map[string]string{"dev/alias.yaml": "services:\n  web: *missing\n"})

		if _, err := stackImages(filepath.Join(dir, "*", "*.yaml")); err == nil {
			t.Error("want error, got nil")
		}
	})

	t.Run("merge keys and empty files pass", func(t *testing.T) {
		dir := t.TempDir()
		writeFiles(t, dir, map[string]string{
			"dev/merge.yaml": "x: &a {k: 1}\ny: &b {l: 2}\nservices:\n  web:\n    <<: [*a, *b]\n    image: example.com/web:1\n",
			"dev/empty.yaml": "",
		})

		images, err := stackImages(filepath.Join(dir, "*", "*.yaml"))
		if err != nil || strings.Join(images, ",") != "example.com/web:1" {
			t.Errorf("want [example.com/web:1], got %v, %v", images, err)
		}
	})

	t.Run("no matching files fails", func(t *testing.T) {
		if _, err := stackImages(filepath.Join(t.TempDir(), "*.yaml")); err == nil {
			t.Error("want error, got nil")
		}
	})
}
