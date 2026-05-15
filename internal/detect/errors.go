// Sentinel errors for the L3 post-commit detection pipeline (Plan 01-07).
//
// Per RESEARCH §Anti-Pattern §1404 (fail-closed when ALL trigger sources
// are unreachable) and PATTERNS Group F lines 104-107, the detection
// surface declares its own typed sentinels so callers branch on
// errors.Is rather than string-matching wrapped errors.
//
//   - ErrAllSourcesUnavailable: returned by the dispatch loop when the
//     30-second poller, the Polaris webhook receiver, AND the S3
//     ObjectCreated SQS consumer all report unreachable. The pool
//     surfaces this so cmd/neksur-server's startup health check can
//     refuse to bind without observability of any trigger surface.
//
//   - ErrSlackPostFailed: returned by alerts.Slack.Post when the Slack
//     incoming webhook returns non-2xx. Alerts are best-effort —
//     callers log + continue; the detection_run row remains as the
//     authoritative record.
//
//   - ErrSampleTooLarge: returned by sampling.StratifiedSample when the
//     caller's budget exceeds the per-bucket ceiling. Surfaces sampling
//     bugs at the call site rather than producing a silently truncated
//     scan.

package detect

import "errors"

var (
	// ErrAllSourcesUnavailable — every trigger source (poller / webhook /
	// s3events) reported unreachable. Phase 1 fail-closed contract per
	// RESEARCH §Anti-Pattern line 1404.
	ErrAllSourcesUnavailable = errors.New("detect: all trigger sources unavailable")

	// ErrSlackPostFailed — the Slack incoming-webhook POST returned
	// non-2xx. Wrapped by alerts.Slack.Post; callers errors.Is to
	// branch.
	ErrSlackPostFailed = errors.New("detect: slack POST failed")

	// ErrSampleTooLarge — sampling.StratifiedSample was given a budget
	// exceeding the per-bucket ceiling. Programmer error; surfacing it
	// here instead of silently truncating.
	ErrSampleTooLarge = errors.New("detect: sample size exceeds bucket cap")
)
