package cmp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/juliosaraiva/patrol-cd/internal/render/cmpwire"
	"github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
)

// match evaluates the discovery rules against the application directory (upstream
// MatchRepository): discover.fileName (filepath.Glob), else discover.find.glob ('**'
// supported), else discover.find.command (supported iff it exits 0 with non-empty
// output). Without rules the plugin is neither supported nor discovery-enabled.
func (s *Server) match(ctx context.Context, req *request) (any, error) {
	d := s.plugin.Spec.Discover
	switch {
	case d.FileName != "":
		matches, err := filepath.Glob(filepath.Join(req.appPath, d.FileName))
		if err != nil {
			return nil, fmt.Errorf("error finding filename match for pattern %q: %w", d.FileName, err)
		}
		return cmpwire.MatchResponse{IsSupported: len(matches) > 0, IsDiscoveryEnabled: true}, nil
	case d.Find.Glob != "":
		matches, err := globDoubleStar(req.appPath, d.Find.Glob)
		if err != nil {
			return nil, fmt.Errorf("error finding glob match for pattern %q: %w", d.Find.Glob, err)
		}
		return cmpwire.MatchResponse{IsSupported: len(matches) > 0, IsDiscoveryEnabled: true}, nil
	case d.Find.IsSet():
		if err := requireAppDir(req); err != nil {
			return nil, err
		}
		out, err := s.runner.run(ctx, "discover", d.Find.Command, req.appPath, req.env)
		if err != nil {
			return nil, fmt.Errorf("error running find command: %w", err)
		}
		return cmpwire.MatchResponse{IsSupported: out != "", IsDiscoveryEnabled: true}, nil
	}
	return cmpwire.MatchResponse{}, nil
}

// globDoubleStar matches pattern ('**' crosses directories) relative to dir without
// following symbolic links during traversal (upstream zglob semantics). Patterns that
// leave dir are rejected.
func globDoubleStar(dir, pattern string) ([]string, error) {
	rel, err := filepath.Rel(dir, filepath.Join(dir, pattern))
	if err != nil {
		return nil, err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("pattern leaves the application directory")
	}
	if !doublestar.ValidatePattern(filepath.ToSlash(rel)) {
		return nil, doublestar.ErrBadPattern
	}
	return doublestar.Glob(os.DirFS(dir), filepath.ToSlash(rel), doublestar.WithNoFollow())
}

// generate runs init (output ignored) and generate in the application directory and
// splits generate's output into documents (upstream GenerateManifest).
func (s *Server) generate(ctx context.Context, req *request) (any, error) {
	if err := requireAppDir(req); err != nil {
		return nil, err
	}
	spec := s.plugin.Spec
	if spec.Init.IsSet() {
		if _, err := s.runner.run(ctx, "init", spec.Init, req.appPath, req.env); err != nil {
			return nil, err
		}
	}
	out, err := s.runner.run(ctx, "generate", spec.Generate, req.appPath, req.env)
	if err != nil {
		return nil, err
	}
	manifests, err := splitManifests([]byte(out))
	if err != nil {
		// The output may contain Secrets: log its size only.
		s.log.Debug("failed to split generated manifests", "app", req.meta.AppName, "outputBytes", len(out))
		return nil, err
	}
	return cmpwire.ManifestResponse{Manifests: manifests}, nil
}

// parameters returns the dynamic announcements (parameters.dynamic stdout, a JSON
// array) followed by the static ones (upstream GetParametersAnnouncement).
func (s *Server) parameters(ctx context.Context, req *request) (any, error) {
	params := s.plugin.Spec.Parameters
	out := []apiclient.ParameterAnnouncement{}
	if params.Dynamic.IsSet() {
		if err := requireAppDir(req); err != nil {
			return nil, err
		}
		stdout, err := s.runner.run(ctx, "parameters", params.Dynamic, req.appPath, req.env)
		if err != nil {
			return nil, fmt.Errorf("error executing dynamic parameter output command: %w", err)
		}
		var dynamic []apiclient.ParameterAnnouncement
		if err := json.Unmarshal([]byte(stdout), &dynamic); err != nil {
			return nil, fmt.Errorf("error unmarshaling dynamic parameter output into ParametersAnnouncementResponse: %w", err)
		}
		out = append(out, dynamic...)
	}
	out = append(out, params.Static...)
	return cmpwire.ParametersResponse{ParameterAnnouncements: out}, nil
}
