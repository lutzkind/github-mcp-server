package github

import (
	"strings"
	"testing"
)

func TestApplyFilePatchOneLinePreservesLargeFile(t *testing.T) {
	before := strings.Join([]string{"header", "target = old", "middle", "footer"}, "\n")
	after, preview, err := applyFilePatch(before, filePatchInput{Operation: "patch_text", Search: "target = old", Replace: "target = new", ExpectedOccurrences: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(after, "header") || !strings.Contains(after, "middle") || !strings.Contains(after, "footer") {
		t.Fatalf("unrelated content was lost: %q", after)
	}
	if preview.MatchedOccurrences != 1 {
		t.Fatalf("matched occurrences = %d", preview.MatchedOccurrences)
	}
}

func TestApplyFilePatchUnifiedDiffMultiHunk(t *testing.T) {
	before := "one\ntwo\nthree\nfour\nfive\nsix"
	patch := "--- a/file.txt\n+++ b/file.txt\n@@ -1,2 +1,2 @@\n one\n-two\n+TWO\n@@ -5,2 +5,2 @@\n five\n-six\n+SIX\n"
	after, preview, err := applyFilePatch(before, filePatchInput{Operation: "unified_diff", Patch: patch})
	if err != nil {
		t.Fatal(err)
	}
	if after != "one\nTWO\nthree\nfour\nfive\nSIX" {
		t.Fatalf("unexpected result: %q", after)
	}
	if len(preview.Hunks) != 2 {
		t.Fatalf("hunks = %#v", preview.Hunks)
	}
}

func TestApplyFilePatchRejectsAmbiguousMalformedAndOverlapping(t *testing.T) {
	if _, _, err := applyFilePatch("x\nx", filePatchInput{Operation: "patch_text", Search: "x", Replace: "y", ExpectedOccurrences: 1}); err == nil {
		t.Fatal("ambiguous patch was accepted")
	}
	if _, _, err := applyFilePatch("one\ntwo", filePatchInput{Operation: "unified_diff", Patch: "@@ -1,1 +1,1 @@\n-one\n"}); err == nil {
		t.Fatal("malformed hunk was accepted")
	}
	patch := "@@ -1,1 +1,1 @@\n-one\n+ONE\n@@ -1,1 +1,1 @@\n-one\n+one\n"
	if _, _, err := applyFilePatch("one", filePatchInput{Operation: "unified_diff", Patch: patch}); err == nil {
		t.Fatal("overlapping hunks were accepted")
	}
}

func TestApplyFilePatchRejectsTruncationAndEmptyResult(t *testing.T) {
	before := strings.Repeat("line\n", 200)
	if _, _, err := applyFilePatch(before, filePatchInput{Operation: "patch_text", Search: "line\n", Replace: "", ExpectedOccurrences: 200}); err == nil {
		t.Fatal("truncated result was accepted")
	}
	if _, _, err := applyFilePatch("substantive", filePatchInput{Operation: "patch_text", Search: "substantive", Replace: "", ExpectedOccurrences: 1}); err == nil {
		t.Fatal("empty result was accepted")
	}
}

func TestBoundedRedactedPreviewRedactsSecrets(t *testing.T) {
	preview := boundedRedactedPreview("API_TOKEN=super-secret-value\nvisible")
	if strings.Contains(preview, "super-secret-value") {
		t.Fatal("secret leaked in preview")
	}
	if !strings.Contains(preview, "REDACTED") {
		t.Fatalf("preview was not redacted: %q", preview)
	}
}
