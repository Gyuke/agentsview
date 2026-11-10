package parser

import (
	"context"
	"os"
	"path/filepath"
	"strings"
)

func newOpenFangProviderFactory(def AgentDef) ProviderFactory {
	return NewSourceSetFactory(
		def,
		openFangProviderCapabilities(),
		func(cfg ProviderConfig) SourceSet {
			return newOpenFangSourceSet(cfg.Roots)
		},
	)
}

func newOpenFangSourceSet(roots []string) JSONLSourceSet {
	return NewJSONLSourceSet(AgentOpenFang, roots,
		WithRecursive(),
		WithContentHashing(),
		WithDescendPath(openFangDescendPath),
		WithIncludePath(isOpenFangSourcePath),
		WithProjectHint(openFangProjectHintFromPath),
		WithSessionIDFromPath(openFangSessionIDFromPath),
		WithLookupIDValid(isValidOpenFangRawID),
		WithRawSessionIDSourceFiles(openFangRawSessionIDSourceFiles),
		WithStoredPathFallbackRoot(openFangStoredPathRoot),
		WithParseFile(openFangParseFile),
		WithForceReplace(),
	)
}

func openFangParseFile(
	_ context.Context, path string, req ParseRequest,
) ([]ParseResult, []string, error) {
	sess, msgs, err := ParseOpenFangSession(
		path, req.Source.ProjectHint, req.Machine,
	)
	if err != nil {
		return nil, nil, err
	}
	if sess == nil {
		return nil, nil, nil
	}
	if req.Fingerprint.Hash != "" {
		sess.File.Hash = req.Fingerprint.Hash
	}
	return []ParseResult{{Session: *sess, Messages: msgs}}, nil, nil
}

func openFangRawSessionIDSourceFiles(roots []string, rawID string) []string {
	var candidates []string
	for _, root := range roots {
		if path := openFangSourceFileForRawID(root, rawID); path != "" {
			candidates = append(candidates, path)
		}
	}
	return candidates
}

func openFangSourceFileForRawID(root, rawID string) string {
	if root == "" {
		return ""
	}
	workspace, sessionID, ok := strings.Cut(rawID, ":")
	if !ok || !IsValidOpenFangWorkspace(workspace) ||
		!IsValidSessionID(sessionID) {
		return ""
	}
	candidate := filepath.Join(
		openfangWorkspacesRoot(root),
		workspace,
		"sessions",
		sessionID+".jsonl",
	)
	if !openFangCandidateUnderRoot(root, candidate) {
		return ""
	}
	if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
		return candidate
	}
	return ""
}

func openFangCandidateUnderRoot(root, candidate string) bool {
	absRoot, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return false
	}
	absCand, err := filepath.Abs(filepath.Clean(candidate))
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(absRoot, absCand)
	if err != nil {
		return false
	}
	rel = filepath.ToSlash(rel)
	return rel != "." && rel != ".." && !strings.HasPrefix(rel, "../")
}

func openFangStoredPathRoot(storedPath string) (string, bool) {
	path := filepath.Clean(storedPath)
	root, ok := openFangImplicitRoot(path)
	if !ok || !isOpenFangSourcePath(root, path) {
		return "", false
	}
	if info, err := os.Stat(path); err != nil || info.IsDir() {
		return "", false
	}
	return root, true
}

func openFangImplicitRoot(path string) (string, bool) {
	// <root>/workspaces/<workspace>/sessions/<uuid>.jsonl
	// <root>/<workspace>/sessions/<uuid>.jsonl
	sessionsDir := filepath.Dir(path)
	if filepath.Base(sessionsDir) != "sessions" {
		return "", false
	}
	workspaceDir := filepath.Dir(sessionsDir)
	if workspaceDir == "" || workspaceDir == "." {
		return "", false
	}
	workspacesOrRoot := filepath.Dir(workspaceDir)
	if filepath.Base(workspacesOrRoot) == "workspaces" {
		root := filepath.Dir(workspacesOrRoot)
		if root == "" || root == "." {
			return "", false
		}
		return root, true
	}
	if workspacesOrRoot == "" || workspacesOrRoot == "." {
		return "", false
	}
	return workspacesOrRoot, true
}

func isOpenFangSourcePath(root, path string) bool {
	parts, ok := openFangSourcePathParts(root, path)
	if !ok {
		return false
	}
	workspace, stem, ok := openFangWorkspaceAndStem(root, parts)
	return ok &&
		IsValidOpenFangWorkspace(workspace) &&
		IsValidSessionID(stem)
}

func openFangProjectHintFromPath(root, path string) string {
	parts, ok := openFangSourcePathParts(root, path)
	if !ok {
		return ""
	}
	workspace, _, ok := openFangWorkspaceAndStem(root, parts)
	if !ok {
		return ""
	}
	return workspace
}

func openFangSessionIDFromPath(root, path string) string {
	parts, ok := openFangSourcePathParts(root, path)
	if !ok {
		return ""
	}
	workspace, stem, ok := openFangWorkspaceAndStem(root, parts)
	if !ok {
		return ""
	}
	return workspace + ":" + stem
}

func openFangWorkspaceAndStem(
	root string, parts []string,
) (string, string, bool) {
	var workspace, filename string
	usesWorkspaces := openFangRootUsesWorkspaces(root)
	switch {
	case usesWorkspaces && len(parts) == 4 &&
		parts[0] == "workspaces" && parts[2] == "sessions":
		workspace = parts[1]
		filename = parts[3]
	case !usesWorkspaces && len(parts) == 3 && parts[1] == "sessions":
		workspace = parts[0]
		filename = parts[2]
	default:
		return "", "", false
	}
	stem, ok := strings.CutSuffix(filename, ".jsonl")
	return workspace, stem, ok
}

func openFangSourcePathParts(root, path string) ([]string, bool) {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	if err != nil {
		return nil, false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return nil, false
		}
	}
	return parts, true
}

func openFangDescendPath(root, path string) bool {
	parts, ok := openFangSourcePathParts(root, path)
	if !ok {
		return false
	}
	switch len(parts) {
	case 1:
		return parts[0] == "workspaces" ||
			(!openFangRootUsesWorkspaces(root) &&
				IsValidOpenFangWorkspace(parts[0]))
	case 2:
		if parts[0] == "workspaces" {
			return IsValidOpenFangWorkspace(parts[1])
		}
		return !openFangRootUsesWorkspaces(root) &&
			IsValidOpenFangWorkspace(parts[0]) &&
			parts[1] == "sessions"
	case 3:
		return openFangRootUsesWorkspaces(root) &&
			parts[0] == "workspaces" &&
			IsValidOpenFangWorkspace(parts[1]) &&
			parts[2] == "sessions"
	default:
		return false
	}
}

func openFangRootUsesWorkspaces(root string) bool {
	clean := filepath.Clean(root)
	if filepath.Base(clean) == "workspaces" {
		return false
	}
	info, err := os.Stat(filepath.Join(clean, "workspaces"))
	return err == nil && info.IsDir()
}

func isValidOpenFangRawID(rawID string) bool {
	workspace, sessionID, ok := strings.Cut(rawID, ":")
	return ok &&
		IsValidOpenFangWorkspace(workspace) &&
		IsValidSessionID(sessionID)
}

func openFangProviderCapabilities() Capabilities {
	source := jsonlFileProviderSourceCapabilities()
	source.ForceReplaceOnParse = CapabilitySupported
	return Capabilities{
		Source: source,
		Content: ContentCapabilities{
			FirstMessage:       CapabilitySupported,
			ToolCalls:          CapabilitySupported,
			ToolResults:        CapabilitySupported,
			MalformedLineCount: CapabilitySupported,
		},
	}
}
