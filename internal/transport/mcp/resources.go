// File resources.go defines the read-only Model Context Protocol (MCP) resource
// descriptors and resource reader exposing the Flowsk Cloudflare Code Mode
// JavaScript SDK Markdown guide on demand.
package mcp

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

const (
	// ResourceURILanguageGuide is the canonical MCP resource URI exposing the
	// complete Flowsk Code Mode TypeScript `.d.ts` SDK specification, search
	// qualifiers, and JavaScript examples so any MCP-capable agent (Claude,
	// Gemini, ChatGPT, Codex, Antigravity) can read it on demand.
	ResourceURILanguageGuide = "flowsk://language/guide"

	// ResourceMIMETextMarkdown is the canonical MIME type for Markdown MCP
	// documentation resources.
	ResourceMIMETextMarkdown = "text/markdown"
)

// ResourceSpec describes an addressable read-only MCP resource advertised by the
// Flowsk server during capability discovery.
type ResourceSpec struct {
	URI         string `json:"uri"`
	Name        string `json:"name"`
	Title       string `json:"title"`
	Description string `json:"description"`
	MIMEType    string `json:"mimeType"`
}

// ResourceContents represents the resolved payload returned when an MCP client
// reads a registered Flowsk resource URI.
type ResourceContents struct {
	URI      string `json:"uri"`
	MIMEType string `json:"mimeType"`
	Text     string `json:"text"`
}

// CanonicalResources returns a defensive copy of the MCP resources exposed by
// Flowsk.
func CanonicalResources() []ResourceSpec {
	specs := []ResourceSpec{
		{
			URI:         ResourceURILanguageGuide,
			Name:        "flowsk-codemode-guide",
			Title:       "Flowsk Code Mode JavaScript SDK Reference & Agent Recipes",
			Description: "Complete TypeScript `.d.ts` specification of the sandboxed `flowsk.*` ES6 JavaScript SDK, GitHub-style search qualifiers, and multi-agent atomic recipes for `flowsk_execute`.",
			MIMEType:    ResourceMIMETextMarkdown,
		},
	}
	return cloneResourceSpecs(specs)
}

// ReadResource validates context liveness and URI membership before resolving
// the requested MCP documentation resource via LanguageGuideProvider.
func (s *ServerSkeleton) ReadResource(ctx context.Context, uri string) (ResourceContents, error) {
	if ctx == nil {
		return ResourceContents{}, errors.New("mcp read resource: nil context")
	}
	if err := ctx.Err(); err != nil {
		return ResourceContents{}, fmt.Errorf("mcp read resource context canceled: %w", err)
	}
	if strings.TrimSpace(uri) != ResourceURILanguageGuide {
		return ResourceContents{}, fmt.Errorf("mcp read resource %w: unknown resource URI %q", ErrInvalidArguments, uri)
	}
	if s.guideProvider == nil {
		return ResourceContents{}, ErrNotImplemented
	}

	out, err := s.guideProvider.Guide(ctx, GuideInput{
		Section:   GuideSectionAll,
		MaxTokens: s.cfg.MaxTokensDefault,
	})
	if err != nil {
		return ResourceContents{}, fmt.Errorf("mcp read resource %q: %w", uri, err)
	}

	mimeType := strings.TrimSpace(out.MIMEType)
	if mimeType == "" {
		mimeType = ResourceMIMETextMarkdown
	}
	return ResourceContents{
		URI:      ResourceURILanguageGuide,
		MIMEType: mimeType,
		Text:     out.Markdown,
	}, nil
}

func cloneResourceSpecs(in []ResourceSpec) []ResourceSpec {
	out := make([]ResourceSpec, len(in))
	copy(out, in)
	return out
}
