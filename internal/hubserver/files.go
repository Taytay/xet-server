package hubserver

// FileSHA256s returns the plain SHA-256 of every file any revision of
// any repo lists - the files `hf upload` committed through this shim.
// The CAS's garbage collector keeps these regardless of the keep list a
// request carries: they have no git repo an operator could list them
// from, and this registry is the only record that they are still
// wanted. Files pushed through the git-lfs bridge are not registered
// here (docs/GIT_LFS.md, known limits), which is why that keep list
// exists.
func (s *Server) FileSHA256s() []string {
	s.mu.RLock()
	repos := make([]*repoState, 0, len(s.repos))
	for _, rs := range s.repos {
		repos = append(repos, rs)
	}
	s.mu.RUnlock()

	var out []string
	seen := make(map[string]bool)
	for _, rs := range repos {
		rs.mu.RLock()
		revisions := make([]*revisionState, 0, len(rs.revisions))
		for _, vs := range rs.revisions {
			revisions = append(revisions, vs)
		}
		rs.mu.RUnlock()
		for _, vs := range revisions {
			vs.mu.RLock()
			for _, ref := range vs.files {
				if ref.SHA256Hex != "" && !seen[ref.SHA256Hex] {
					seen[ref.SHA256Hex] = true
					out = append(out, ref.SHA256Hex)
				}
			}
			vs.mu.RUnlock()
		}
	}
	return out
}
