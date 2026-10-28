package store

import "context"

// MediaSession reports whether vendor's session id was recorded by the
// child of an image or video borrow aggy ran. aggy image --edit resumes
// only such a session, never one a sidecar from a cloned repo names.
func (s *Store) MediaSession(ctx context.Context, vendor, sessionID string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM children c JOIN borrows b ON b.child_id = c.id
		 WHERE c.vendor = ? AND c.vendor_session_id = ? AND c.vendor_session_id <> ''
		   AND b.kind IN ('image', 'video')`, vendor, sessionID).Scan(&n)
	return n > 0, err
}
