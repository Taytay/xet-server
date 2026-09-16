package hubserver

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/guilt/xet-server/internal/auth"
)

// syncLockDirs copies every file each dir lacks from the other: an
// additive sync tool meeting two replicas' lock directories.
func syncLockDirs(t *testing.T, a, b string) {
	t.Helper()
	copyMissing := func(src, dst string) {
		filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return err
			}
			rel, _ := filepath.Rel(src, path)
			target := filepath.Join(dst, rel)
			if _, err := os.Stat(target); err == nil {
				return nil
			}
			os.MkdirAll(filepath.Dir(target), 0o755)
			data, _ := os.ReadFile(path)
			return os.WriteFile(target, data, 0o644)
		})
	}
	copyMissing(a, b)
	copyMissing(b, a)
}

func TestFolderLocks_ElectionAcrossReplicas(t *testing.T) {
	dirA, dirB := t.TempDir(), t.TempDir()
	a, _ := newFolderLockStore(dirA)
	b, _ := newFolderLockStore(dirB)
	const repo = "team/game.git"
	const path = "Assets/Scenes/Main.unity"

	// Alice on A and Bob on B both lock the scene while their folders
	// are apart; each replica, knowing nothing better, grants it.
	alice, existing, err := a.Create(repo, lfsLock{ID: "aaaa", Path: path, LockedAt: "2026-09-16T10:00:00Z", Owner: lfsLockOwner{"alice"}})
	if err != nil || existing != nil || alice == nil {
		t.Fatalf("alice create: %v %v %v", alice, existing, err)
	}
	bob, existing, err := b.Create(repo, lfsLock{ID: "bbbb", Path: path, LockedAt: "2026-09-16T10:00:05Z", Owner: lfsLockOwner{"bob"}})
	if err != nil || existing != nil || bob == nil {
		t.Fatalf("bob create: %v %v %v", bob, existing, err)
	}

	// Once the claims meet, both replicas elect the same holder: the
	// earlier claim. Bob's claim is superseded, not deleted.
	syncLockDirs(t, dirA, dirB)
	for name, store := range map[string]*folderLockStore{"A": a, "B": b} {
		held, err := store.List(repo)
		if err != nil || len(held) != 1 || held[0].ID != "aaaa" {
			t.Fatalf("%s after sync: List = %+v, %v; want alice's lock alone", name, held, err)
		}
		if l, _ := store.Lookup(repo, "bbbb"); l == nil || l.Owner.Name != "bob" {
			t.Fatalf("%s: bob's superseded claim not found by id", name)
		}
		// A third person cannot take the path on either replica.
		if _, existing, _ := store.Create(repo, lfsLock{ID: "cccc", Path: path, LockedAt: "2026-09-16T09:00:00Z", Owner: lfsLockOwner{"carol"}}); existing == nil || existing.ID != "aaaa" {
			t.Fatalf("%s: carol's create did not conflict with alice's lock: %+v", name, existing)
		}
	}

	// Alice unlocks on A; after sync, Bob's claim is the holder on both.
	if err := a.Remove(repo, "aaaa"); err != nil {
		t.Fatal(err)
	}
	syncLockDirs(t, dirA, dirB)
	for name, store := range map[string]*folderLockStore{"A": a, "B": b} {
		held, _ := store.List(repo)
		if len(held) != 1 || held[0].ID != "bbbb" {
			t.Fatalf("%s after alice's unlock: List = %+v; want bob's claim", name, held)
		}
		if l, _ := store.Lookup(repo, "aaaa"); l != nil {
			t.Fatalf("%s: alice's released lock still found", name)
		}
	}

	// Bob unlocks on B; both replicas end empty. Nothing was ever
	// deleted or modified in either directory.
	b.Remove(repo, "bbbb")
	syncLockDirs(t, dirA, dirB)
	for name, store := range map[string]*folderLockStore{"A": a, "B": b} {
		if held, _ := store.List(repo); len(held) != 0 {
			t.Fatalf("%s at the end: List = %+v; want none", name, held)
		}
	}
	entries, _ := os.ReadDir(filepath.Join(dirA, "team%2Fgame.git"))
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	want := []string{"aaaa.lock.json", "aaaa.unlock", "bbbb.lock.json", "bbbb.unlock"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("lock dir holds %v, want %v (claims and tombstones, nothing removed)", names, want)
	}
}

func TestFolderLocks_TieBreaksByIDAndIgnoresPartialFiles(t *testing.T) {
	dir := t.TempDir()
	store, _ := newFolderLockStore(dir)
	const repo = "r"
	store.Create(repo, lfsLock{ID: "zzzz", Path: "p", LockedAt: "2026-09-16T10:00:00Z", Owner: lfsLockOwner{"z"}})
	// A claim with the same timestamp synced in from elsewhere.
	writeOnce(filepath.Join(store.repoDir(repo), "aaaa"+lockClaimSuffix), lfsLock{ID: "aaaa", Path: "p", LockedAt: "2026-09-16T10:00:00Z", Owner: lfsLockOwner{"a"}})
	// A half-synced claim file and a temp file.
	os.WriteFile(filepath.Join(store.repoDir(repo), "half"+lockClaimSuffix), []byte(`{"id":"ha`), 0o644)
	os.WriteFile(filepath.Join(store.repoDir(repo), "x.lock.json.tmp-123"), []byte(`{}`), 0o644)

	held, err := store.List(repo)
	if err != nil || len(held) != 1 || held[0].ID != "aaaa" {
		t.Fatalf("List = %+v, %v; want the lexically smaller id to win the tie", held, err)
	}
}

// A hub on the folder store answers lock verification from the files,
// so a replica sees a lock taken on another machine as "theirs".
func TestFolderLocks_VerifySeesOtherReplicasLock(t *testing.T) {
	dirA, dirB := t.TempDir(), t.TempDir()
	newHub := func(dir string) *httptest.Server {
		a := auth.NewSignedTokenAuth(lfsFixtureSecret)
		h := New("http://cas.example:8420", newFakeCAS())
		h.SetAuthenticator(a)
		h.SetTokenMinter(a)
		if err := h.SetLockDir(dir); err != nil {
			t.Fatal(err)
		}
		ts := httptest.NewServer(h)
		t.Cleanup(ts.Close)
		return ts
	}
	hubA, hubB := newHub(dirA), newHub(dirB)
	locks := "/team/game.git/info/lfs/locks"

	resp, _ := lfsDo(t, hubA, http.MethodPost, locks, "alice", `{"path":"Main.unity"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("alice lock on A: %d", resp.StatusCode)
	}
	syncLockDirs(t, dirA, dirB)
	resp, body := lfsDo(t, hubB, http.MethodPost, locks+"/verify", "bob", `{}`)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"theirs":[{`) || !strings.Contains(string(body), `"alice"`) {
		t.Fatalf("verify on B: %d %s; want alice's lock under theirs", resp.StatusCode, body)
	}
	resp, _ = lfsDo(t, hubB, http.MethodPost, locks, "bob", `{"path":"Main.unity"}`)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("bob lock on B after sync: %d, want 409", resp.StatusCode)
	}
}
