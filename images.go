// SPDX-License-Identifier: Apache-2.0

package plugin

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"github.com/rs/zerolog/log"
	"go.yaml.in/yaml/v3"
)

// stackFile is the part of a compose/stack file the image check reads.
type stackFile struct {
	Services map[string]struct {
		Image string `yaml:"image"`
	} `yaml:"services"`
}

// imageChecker verifies that images exist in their registries.
type imageChecker struct {
	hosts     []string
	cred      authn.Authenticator
	transport http.RoundTripper
}

// checkImages verifies that every image referenced by the stack files exists.
func (p *Plugin) checkImages(ctx context.Context) error {
	images, err := stackImages(p.Settings.StackPath)
	if err != nil {
		return err
	}

	checker := imageChecker{
		hosts:     p.Settings.RegistryHosts,
		cred:      authn.Anonymous,
		transport: p.HTTPClient().Transport,
	}
	if p.Settings.RegistryUsername != "" {
		checker.cred = &authn.Basic{Username: p.Settings.RegistryUsername, Password: p.Settings.RegistryPassword}
	}

	return checker.check(ctx, images)
}

// stackFiles returns every .yaml/.yml file not in dot-directories, e.g. .git.
func stackFiles(pattern string) ([]string, error) {
	if pattern != "" {
		return filepath.Glob(pattern)
	}

	var files []string

	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case d.IsDir() && path != "." && strings.HasPrefix(d.Name(), "."):
			return filepath.SkipDir
		case !d.IsDir() && (filepath.Ext(path) == ".yaml" || filepath.Ext(path) == ".yml"):
			files = append(files, path)
		}

		return nil
	})

	return files, err
}

// stackImages returns the sorted, unique images referenced by the stack files.
func stackImages(pattern string) ([]string, error) {
	files, err := stackFiles(pattern)
	if err != nil {
		return nil, fmt.Errorf("failed to list stack files %q: %w", pattern, err)
	}

	if len(files) == 0 {
		return nil, fmt.Errorf("no stack files found (stack-path %q)", pattern)
	}

	seen := map[string]struct{}{}

	for _, file := range files {
		data, err := os.ReadFile(file) //nolint:gosec // G304: the paths come from the pipeline's own workspace
		if err != nil {
			return nil, fmt.Errorf("failed to read %s: %w", file, err)
		}

		doc, err := lintYAML(data)
		if err != nil {
			return nil, fmt.Errorf("invalid YAML in %s: %w", file, err)
		}

		var stack stackFile
		if err := doc.Decode(&stack); err != nil {
			log.Warn().Msgf("skipping %s: not a stack file: %v", file, err)

			continue
		}

		for _, svc := range stack.Services {
			if svc.Image != "" {
				seen[svc.Image] = struct{}{}
			}
		}
	}

	return slices.Sorted(maps.Keys(seen)), nil
}

// lintYAML parses data and fails on what yamllint's relaxed preset treats as errors.
func lintYAML(data []byte) (*yaml.Node, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}

	return &doc, duplicateKey(&doc)
}

// duplicateKey returns an error for the first mapping in the tree that repeats a key.
func duplicateKey(n *yaml.Node) error {
	if n.Kind == yaml.MappingNode {
		if err := uniqueKeys(n); err != nil {
			return err
		}
	}

	for _, child := range n.Content {
		if err := duplicateKey(child); err != nil {
			return err
		}
	}

	return nil
}

// uniqueKeys returns an error if the mapping defines a key twice.
func uniqueKeys(mapping *yaml.Node) error {
	firstLine := map[string]int{}

	for pair := range slices.Chunk(mapping.Content, 2) {
		key := pair[0]
		if line, seen := firstLine[key.Value]; seen {
			return fmt.Errorf("line %d: duplicate key %q, first defined at line %d", key.Line, key.Value, line)
		}

		firstLine[key.Value] = key.Line
	}

	return nil
}

// check looks up every image's manifest and returns an error listing the ones that failed.
func (c imageChecker) check(ctx context.Context, images []string) error {
	var failed []string

	for _, image := range images {
		if strings.Contains(image, "${") {
			log.Warn().Msgf("Image %s skipped: contains a variable", image)

			continue
		}

		if err := c.head(ctx, image); err != nil {
			log.Error().Msgf("Image %s %s", image, failureReason(err))
			failed = append(failed, image)

			continue
		}

		log.Info().Msgf("Image %s exists", image)
	}

	if len(failed) > 0 {
		return fmt.Errorf("%d image(s) not found or not accessible: %s", len(failed), strings.Join(failed, ", "))
	}

	return nil
}

// head performs a HEAD request for the given image reference to check its existence and accessibility.
func (c imageChecker) head(ctx context.Context, image string) error {
	ref, err := name.ParseReference(image)
	if err != nil {
		return err
	}

	auth := authn.Anonymous
	if slices.Contains(c.hosts, ref.Context().RegistryStr()) {
		auth = c.cred
	}

	opts := []remote.Option{remote.WithContext(ctx), remote.WithAuth(auth)}
	if c.transport != nil {
		opts = append(opts, remote.WithTransport(c.transport))
	}

	_, err = remote.Head(ref, opts...)

	return err
}

// failureReason returns a human-readable explanation for why an image lookup failed.
func failureReason(err error) string {
	if terr, ok := errors.AsType[*transport.Error](err); ok {
		switch terr.StatusCode {
		case http.StatusNotFound:
			return "not found"
		case http.StatusUnauthorized, http.StatusForbidden:
			return "not accessible: the registry refused access (is its host in registry-hosts?)"
		case http.StatusTooManyRequests:
			return "not checked: the registry rate limit was reached"
		}
	}

	return fmt.Sprintf("check failed: %v", err)
}
