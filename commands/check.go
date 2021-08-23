package commands

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"runtime"

	resource "github.com/concourse/registry-image-resource"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"github.com/sirupsen/logrus"
)

type CheckRequest struct {
	Source  resource.Source   `json:"source"`
	Version *resource.Version `json:"version"`
}

type CheckResponse []resource.Version

type check struct {
	stdin  io.Reader
	stderr io.Writer
	stdout io.Writer
	args   []string
}

func NewCheck(
	stdin io.Reader,
	stderr io.Writer,
	stdout io.Writer,
	args []string,
) *check {
	return &check{
		stdin:  stdin,
		stderr: stderr,
		stdout: stdout,
		args:   args,
	}
}

func (c *check) Execute() error {
	setupLogging(c.stderr)

	var req CheckRequest
	decoder := json.NewDecoder(c.stdin)
	decoder.DisallowUnknownFields()
	err := decoder.Decode(&req)
	if err != nil {
		return fmt.Errorf("invalid payload: %s", err)
	}

	if req.Source.AwsAccessKeyId != "" && req.Source.AwsSecretAccessKey != "" && req.Source.AwsRegion != "" {
		if !req.Source.AuthenticateToECR() {
			return fmt.Errorf("cannot authenticate with ECR")
		}
	}

	repo, err := name.NewRepository(req.Source.Repository, name.WeakValidation)
	if err != nil {
		return fmt.Errorf("failed to resolve repository: %s", err)
	}

	var response CheckResponse
	tag := new(name.Tag)

	// only use the RegistryMirror as the Registry if the repo doesn't use a different,
	// explicitly-declared, non-default registry, such as 'some-registry.com/foo/bar'.
	if req.Source.RegistryMirror != nil && repo.Registry.String() == name.DefaultRegistry {
		mirror, err := name.NewRepository(repo.String())
		if err != nil {
			return fmt.Errorf("could not resolve mirror repository: %s", err)
		}

		mirror.Registry, err = name.NewRegistry(req.Source.RegistryMirror.Host, name.WeakValidation)
		if err != nil {
			return fmt.Errorf("could not resolve registry: %s", err)
		}

		*tag = mirror.Tag(req.Source.Tag())

		response, err = performCheck(req.Source.RegistryMirror.BasicCredentials, req.Version, *tag)
		if err != nil {
			logrus.Warnf("checking mirror %s failed: %s", mirror.RegistryStr(), err)
		} else if len(response) == 0 {
			logrus.Warnf("checking mirror %s failed: tag not found", mirror.RegistryStr())
		}
	}

	if len(response) == 0 {
		*tag = repo.Tag(req.Source.Tag())
		response, err = performCheck(req.Source.BasicCredentials, req.Version, *tag)
	}
	if err != nil {
		return fmt.Errorf("checking origin %s failed: %s", tag.RegistryStr(), err)
	}

	err = json.NewEncoder(c.stdout).Encode(response)
	if err != nil {
		return fmt.Errorf("could not marshal JSON: %s", err)
	}

	return nil
}

func performCheck(principal resource.BasicCredentials, version *resource.Version, ref name.Tag) (CheckResponse, error) {
	auth := &authn.Basic{
		Username: principal.Username,
		Password: principal.Password,
	}

	imageOpts := []remote.Option{}

	imageOpts = append(imageOpts, remote.WithPlatform(v1.Platform{
		Architecture: runtime.GOARCH,
		OS:           runtime.GOOS,
	}))

	if auth.Username != "" && auth.Password != "" {
		imageOpts = append(imageOpts, remote.WithAuth(auth))
	}

	digest, found, err := headOrGet(ref, imageOpts...)
	if err != nil {
		return CheckResponse{}, fmt.Errorf("get remote image: %w", err)
	}

	response := CheckResponse{}
	if version != nil && found && version.Digest != digest.String() {
		digestRef := ref.Repository.Digest(version.Digest)

		_, found, err := headOrGet(digestRef, imageOpts...)
		if err != nil {
			return CheckResponse{}, fmt.Errorf("get remote image: %w", err)
		}

		if found {
			response = append(response, *version)
		}
	}

	if found {
		response = append(response, resource.Version{
			Digest: digest.String(),
		})
	}

	return response, nil
}

func headOrGet(ref name.Reference, imageOpts ...remote.Option) (v1.Hash, bool, error) {
	v1Desc, err := remote.Head(ref, imageOpts...)
	if err != nil {
		if checkMissingManifest(err) {
			return v1.Hash{}, false, nil
		}

		remoteDesc, err := remote.Get(ref, imageOpts...)
		if err != nil {
			if checkMissingManifest(err) {
				return v1.Hash{}, false, nil
			}

			return v1.Hash{}, false, err
		}

		return remoteDesc.Digest, true, nil
	}

	return v1Desc.Digest, true, nil
}

func checkMissingManifest(err error) bool {
	if rErr, ok := err.(*transport.Error); ok {
		return rErr.StatusCode == http.StatusNotFound
	}

	return false
}
