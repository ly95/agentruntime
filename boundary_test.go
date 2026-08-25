package agentruntime

import (
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

func TestPackageDoesNotImportHostApplications(t *testing.T) {
	allowedPrefixes := []string{
		"github.com/ly95/agentruntime",
		"github.com/openai/openai-go/v3",
		"github.com/santhosh-tekuri/jsonschema/v6",
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("ParseFile(%s): %v", name, err)
		}
		for _, spec := range file.Imports {
			importPath, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatalf("Unquote import in %s: %v", name, err)
			}
			if isAllowedRuntimeImport(importPath, allowedPrefixes) {
				continue
			}
			t.Fatalf("%s imports non-runtime package %q", name, importPath)
		}
	}
}

func isAllowedRuntimeImport(importPath string, allowedPrefixes []string) bool {
	firstComponent, _, _ := strings.Cut(importPath, "/")
	if !strings.Contains(firstComponent, ".") {
		return true
	}
	for _, prefix := range allowedPrefixes {
		if importPath == prefix || strings.HasPrefix(importPath, prefix+"/") {
			return true
		}
	}
	return false
}
