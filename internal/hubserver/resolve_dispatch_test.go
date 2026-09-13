package hubserver

import "testing"

// TestParseResolvePath_AllRepoTypePrefixes pins that the resolve-dispatch
// path parser accepts every real Hub URL shape huggingface_hub can send,
// present or future: models (no prefix), datasets ("datasets/" prefix),
// spaces ("spaces/" prefix), and any unrecognised leading segment that a
// future repo type might introduce - all extract owner/name from the two
// segments IMMEDIATELY before "/resolve/", so no per-type code change is
// needed as HF adds new URL kinds. Regression test for a real bug where
// only the model shape parsed and every dataset download `hf download
// bigcode/the-stack-v2 ... --repo-type dataset` 404'd here.
func TestParseResolvePath_AllRepoTypePrefixes(t *testing.T) {
	cases := []struct {
		name         string
		rest         string
		wantRepoType string
		wantRepoID   string
		wantRevision string
		wantFilename string
	}{
		{"model", "guilt/xet-model-library/resolve/main/model.gguf", "model", "guilt/xet-model-library", "main", "model.gguf"},
		{"dataset", "datasets/bigcode/the-stack-v2/resolve/main/data/Python/train-00000-of-00009.parquet", "dataset", "bigcode/the-stack-v2", "main", "data/Python/train-00000-of-00009.parquet"},
		{"space", "spaces/alice/demo/resolve/main/app.py", "space", "alice/demo", "main", "app.py"},
		{"unknown_prefix_falls_back_to_model", "buckets/alice/bkt/resolve/main/file.bin", "model", "alice/bkt", "main", "file.bin"},
		{"nested_filename", "alice/model/resolve/refs/pr/1/file.bin", "model", "alice/model", "refs", "pr/1/file.bin"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotType, gotRepo, gotRev, gotFile, ok := parseResolvePath(tc.rest)
			if !ok {
				t.Fatalf("parseResolvePath(%q) = _, _, _, _, false; want true", tc.rest)
			}
			if gotType != tc.wantRepoType {
				t.Errorf("repoType = %q, want %q", gotType, tc.wantRepoType)
			}
			if gotRepo != tc.wantRepoID {
				t.Errorf("repoID = %q, want %q", gotRepo, tc.wantRepoID)
			}
			if gotRev != tc.wantRevision {
				t.Errorf("revision = %q, want %q", gotRev, tc.wantRevision)
			}
			if gotFile != tc.wantFilename {
				t.Errorf("filename = %q, want %q", gotFile, tc.wantFilename)
			}
		})
	}
}

// TestParseResolvePath_Rejects checks the parser rejects (returns ok=false)
// paths that don't look like a resolve URL at all - no /resolve/ marker,
// only one segment before it, or empty filename after the revision.
func TestParseResolvePath_Rejects(t *testing.T) {
	cases := []string{
		"",
		"guilt/xet-model-library",
		"resolve/main/file.bin", // /resolve/ at start: not enough segments before it
		"a/b/resolve/main",      // no filename after revision
		"a/b/resolve/main/",     // empty filename
	}
	for _, tc := range cases {
		t.Run(tc, func(t *testing.T) {
			if _, _, _, _, ok := parseResolvePath(tc); ok {
				t.Errorf("parseResolvePath(%q) ok=true; want false", tc)
			}
		})
	}
}

// TestExtractRepoIDBefore covers the LFS-batch dispatch's repo-ID
// extraction, which parallels parseResolvePath's owner/name rule (last
// two segments before the suffix), so a dataset LFS-batch URL under
// /datasets/{owner}/{name}.git/info/lfs/objects/batch reaches the batch
// handler with repo_id="{owner}/{name}".
func TestExtractRepoIDBefore(t *testing.T) {
	cases := []struct {
		name string
		rest string
		want string
	}{
		{"model_batch", "alice/foo.git/info/lfs/objects/batch", "alice/foo.git"},
		{"dataset_batch", "datasets/alice/foo.git/info/lfs/objects/batch", "alice/foo.git"},
		{"space_batch", "spaces/alice/foo.git/info/lfs/objects/batch", "alice/foo.git"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractRepoIDBefore(tc.rest, lfsBatchPathSuffix)
			if got != tc.want {
				t.Errorf("extractRepoIDBefore(%q) = %q, want %q", tc.rest, got, tc.want)
			}
		})
	}
}
