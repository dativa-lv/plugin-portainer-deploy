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

// stackFile is the part of a compose/stack file the image check and the stack rules read.
type stackFile struct {
	Services map[string]stackService `yaml:"services"`
}

type stackService struct {
	Image  string        `yaml:"image"`
	Deploy serviceDeploy `yaml:"deploy"`
}

// serviceDeploy holds the deploy settings the stack rules check. Values that may be
// written as ${VARIABLE} are decoded as any, so a variable doesn't make the file unreadable.
type serviceDeploy struct {
	Mode      string `yaml:"mode"`
	Replicas  any    `yaml:"replicas"`
	Resources struct {
		Limits struct {
			CPUs   any `yaml:"cpus"`
			Memory any `yaml:"memory"`
		} `yaml:"limits"`
	} `yaml:"resources"`
}

// loadedStack is a parsed stack file and the path it was read from.
type loadedStack struct {
	file  string
	stack stackFile
}

// imageVerifier checks that images exist in their registries.
type imageVerifier struct {
	privateRegistries []string
	credentials       authn.Authenticator
	transport         http.RoundTripper
}

// checkImages applies the stack rules and verifies that every image referenced by the stack files exists.
func (p *Plugin) checkImages(ctx context.Context) error {
	stacks, err := loadStacks(p.Settings.StackPath)
	if err != nil {
		return err
	}

	rulesErr := lintStacks(stacks)

	privateRegistries, err := normalizeRegistryHosts(p.Settings.RegistryHosts)
	if err != nil {
		return err
	}

	verifier := imageVerifier{
		privateRegistries: privateRegistries,
		credentials:       authn.Anonymous,
		transport:         p.HTTPClient().Transport,
	}

	if p.Settings.RegistryUsername != "" {
		verifier.credentials = &authn.Basic{
			Username: p.Settings.RegistryUsername,
			Password: p.Settings.RegistryPassword,
		}
	}

	return errors.Join(rulesErr, verifier.verifyAll(ctx, stackImages(stacks)))
}

// normalizeRegistryHosts converts hosts to the names image references resolve to.
func normalizeRegistryHosts(hosts []string) ([]string, error) {
	registries := make([]string, 0, len(hosts))

	for _, host := range hosts {
		registry, err := name.NewRegistry(host)
		if err != nil {
			return nil, fmt.Errorf("invalid registry-hosts entry %q: %w", host, err)
		}

		registries = append(registries, registry.RegistryStr())
	}

	return registries, nil
}

// findStackFiles returns the files matching pattern or, when pattern is empty,
// every YAML file in the workspace outside hidden directories (.git, .woodpecker).
func findStackFiles(pattern string) ([]string, error) {
	if pattern != "" {
		return filepath.Glob(pattern)
	}

	var files []string

	err := filepath.WalkDir(".", func(path string, entry fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case isHiddenDir(path, entry):
			return filepath.SkipDir
		case isYAMLFile(path, entry):
			files = append(files, path)
		}

		return nil
	})

	return files, err
}

func isHiddenDir(path string, entry fs.DirEntry) bool {
	return entry.IsDir() && path != "." && strings.HasPrefix(entry.Name(), ".")
}

func isYAMLFile(path string, entry fs.DirEntry) bool {
	extension := filepath.Ext(path)

	return !entry.IsDir() && (extension == ".yaml" || extension == ".yml")
}

// collectStackImages returns the sorted, unique images referenced by the stack files.
func collectStackImages(pattern string) ([]string, error) {
	stacks, err := loadStacks(pattern)
	if err != nil {
		return nil, err
	}

	return stackImages(stacks), nil
}

// stackImages returns the sorted, unique images referenced by the stacks.
func stackImages(stacks []loadedStack) []string {
	uniqueImages := map[string]struct{}{}

	for _, loaded := range stacks {
		for _, service := range loaded.stack.Services {
			if service.Image != "" {
				uniqueImages[service.Image] = struct{}{}
			}
		}
	}

	return slices.Sorted(maps.Keys(uniqueImages))
}

// loadStacks parses the stack files matching pattern. Invalid YAML is an error;
// valid YAML that is not a stack file is skipped with a warning.
func loadStacks(pattern string) ([]loadedStack, error) {
	files, err := findStackFiles(pattern)
	if err != nil {
		return nil, fmt.Errorf("failed to list stack files %q: %w", pattern, err)
	}

	if len(files) == 0 {
		return nil, fmt.Errorf("no stack files found (stack-path %q)", pattern)
	}

	stacks := make([]loadedStack, 0, len(files))

	for _, file := range files {
		content, err := os.ReadFile(file) //nolint:gosec // G304: the paths come from the pipeline's own workspace
		if err != nil {
			return nil, fmt.Errorf("failed to read %s: %w", file, err)
		}

		// Decoding into any reports only syntax errors (indentation, duplicate keys, undefined aliases).
		if err := yaml.Unmarshal(content, new(any)); err != nil {
			return nil, fmt.Errorf("invalid YAML in %s: %w", file, err)
		}

		// Valid YAML of another shape (e.g. an app config with a "services" list) is not a stack file.
		var stack stackFile
		if err := yaml.Unmarshal(content, &stack); err != nil {
			log.Warn().Msgf("skipping %s: not a stack file: %v", file, err)

			continue
		}

		stacks = append(stacks, loadedStack{file: file, stack: stack})
	}

	return stacks, nil
}

// verifyAll checks every image and returns an error listing the ones that failed.
func (v imageVerifier) verifyAll(ctx context.Context, images []string) error {
	var failedImages []string

	for _, image := range images {
		if strings.Contains(image, "$") {
			log.Warn().Msgf("Image %s skipped: contains a variable", image)

			continue
		}

		if err := v.verify(ctx, image); err != nil {
			log.Error().Msgf("Image %s %s", image, failureReason(err))

			failedImages = append(failedImages, image)

			continue
		}

		log.Info().Msgf("Image %s exists", image)
	}

	if len(failedImages) > 0 {
		return fmt.Errorf("%d image(s) not found or not accessible: %s",
			len(failedImages), strings.Join(failedImages, ", "))
	}

	return nil
}

// verify checks that the image's manifest exists.
func (v imageVerifier) verify(ctx context.Context, image string) error {
	reference, err := name.ParseReference(image)
	if err != nil {
		return err
	}

	options := []remote.Option{
		remote.WithContext(ctx),
		remote.WithAuth(v.credentialsFor(reference.Context().RegistryStr())),
	}

	if v.transport != nil {
		options = append(options, remote.WithTransport(v.transport))
	}

	_, err = remote.Head(reference, options...)

	return err
}

// credentialsFor returns the credentials for a private registry and anonymous access for any other.
func (v imageVerifier) credentialsFor(registry string) authn.Authenticator {
	if slices.Contains(v.privateRegistries, registry) {
		return v.credentials
	}

	return authn.Anonymous
}

// failureReason explains why an image check failed.
func failureReason(err error) string {
	if registryErr, ok := errors.AsType[*transport.Error](err); ok {
		switch registryErr.StatusCode {
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
