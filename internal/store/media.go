package store

import "context"

// MediaSession reports whether vendor's session id was recorded by the
// child of a borrow of kind (image or video) that aggy ran for the repo
// at dir. aggy image --edit resumes only such a session: never one a
// sidecar from a cloned repo names, one made for another repo, or a video
// session.
func (s *Store) MediaSession(ctx context.Context, dir, vendor, kind, sessionID string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM children c
		   JOIN borrows b ON b.child_id = c.id
		   JOIN parents p ON p.id = c.parent_id
		 WHERE c.vendor = ? AND c.vendor_session_id = ? AND c.vendor_session_id <> ''
		   AND b.kind = ? AND c.kind = ? AND p.dir = ?`, vendor, sessionID, kind, kind, dir).Scan(&n)
	return n > 0, err
}
