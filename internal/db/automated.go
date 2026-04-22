package db

import (
	"slices"
	"strings"
)

// IsAutomatedBackfillMarker is the stats/sync_metadata key that
// gates the one-time is_automated re-classification. Bump the
// suffix whenever the classifier patterns change so existing
// databases re-run the backfill on next open.
const IsAutomatedBackfillMarker = "is_automated_backfill_v2"

// automatedPrefixes are first_message prefixes that identify
// automated (roborev) sessions. Matched case-sensitively.
// Combined with the single-turn gate (user_message_count <= 1)
// to avoid misclassifying interactive sessions.
var automatedPrefixes = []string{
	"You are a code reviewer.",
	"You are a security code reviewer.",
	"You are a design reviewer.",
	"You are a code assistant. Your task is to address",
	"You are a code review insights analyst.",
	"You are reviewing whether an implementation matches",
	"You are a plan document reviewer.",
	"You are a spec document reviewer.",
	"You are summarizing a day of AI agent activity.",
	"You are analyzing AI agent sessions.",
	"## Analysis Request",
	"# Fix Request",
	"You are a helpful assistant working on a software project.",
	"You are combining multiple code review outputs into a single GitHub PR comment.",
}

// automatedSubstrings are patterns matched anywhere in the
// first message. Used for catch-all markers embedded in
// longer prompts.
var automatedSubstrings = []string{
	"invoked by roborev to perform this review",
	"You are a conversation title generator",
}

// automatedExactMatches are first messages that, after trimming
// surrounding whitespace, exactly equal one of these strings.
// Used for prompts too generic for prefix or substring matching
// (e.g., a single-word warmup ping).
var automatedExactMatches = []string{
	"Warmup",
}

// IsAutomatedSession returns true if the first message
// matches a known automated review/fix prompt pattern.
func IsAutomatedSession(firstMessage string) bool {
	for _, prefix := range automatedPrefixes {
		if strings.HasPrefix(firstMessage, prefix) {
			return true
		}
	}
	for _, sub := range automatedSubstrings {
		if strings.Contains(firstMessage, sub) {
			return true
		}
	}
	trimmed := strings.TrimSpace(firstMessage)
	return slices.Contains(automatedExactMatches, trimmed)
}
