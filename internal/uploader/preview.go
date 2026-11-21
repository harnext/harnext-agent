package uploader

import "github.com/harnext/harnext-agent/internal/aggregate"

// PreviewOptions selects what Preview shows.
type PreviewOptions struct {
	AgentVersion  string
	ConfigVersion int64
	Gate          func(aggregate.Row) Decision // same meaning as Options.Gate; nil shows nothing
	// MaxSessions limits the sessions in the body, newest first; 0 or less shows all of them.
	MaxSessions int
}

// PreviewResult is the request body that Sync would send, plus counts.
type PreviewResult struct {
	Body     []byte // one ingest request body; exactly what the uploader posts, without a network call
	Sessions int    // sessions in Body
	Total    int    // valid sessions before MaxSessions applied
	Invalid  int    // sessions outside the contract limits; never sent
}

// Preview builds the ingest body for the newest valid sessions without contacting the server or the ledger.
func Preview(rows []aggregate.Row, o PreviewOptions) PreviewResult {
	valid, invalid, _ := prepare(newestSessionsFirst(rows), o.ConfigVersion, o.Gate)
	res := PreviewResult{Total: len(valid), Invalid: invalid}
	if o.MaxSessions > 0 && len(valid) > o.MaxSessions {
		valid = valid[:o.MaxSessions]
	}
	res.Sessions = len(valid)
	res.Body = encodeBody(o.AgentVersion, o.ConfigVersion, valid)
	return res
}
