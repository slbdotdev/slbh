// Package secretarywake queues a pointer message to the interactive Codex
// session that serves as the Secretary.
//
// Waking is best-effort and idempotent in effect. A failed or repeated wake
// cannot lose a report: report contents remain durable in the org store and
// pending until the Secretary acknowledges them.
package secretarywake
