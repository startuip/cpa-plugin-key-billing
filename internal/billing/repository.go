package billing

import "time"

type Repository interface {
	OpenReferencePrices() (ReferencePriceRepository, error)
	Load(requestEventCutoff, pluginLogCutoff time.Time) (Snapshot, error)
	Save(state *State, changes Changes) error
	UpsertPrice(price CustomPrice) error
	DeletePrice(modelID string) error

	EventKeys(from, to, since time.Time) ([]EventKey, error)
	RequestEvents(query RequestEventQuery, since time.Time) (RequestEventView, error)
	RequestErrors(query RequestErrorQuery, since time.Time) (RequestErrorView, error)
	Analysis(query RequestEventQuery, since time.Time) (AnalysisView, error)

	AppendPluginLog(entry PluginLog, cutoff time.Time) error
	PluginLogsPage(query PluginLogQuery) (PluginLogPage, error)
	ClearPluginLogs() (int, error)

	Close() error
}

type Snapshot struct {
	State             *State
	RequestEventCount int
}

type Changes struct {
	// Keys lists the scopes to upsert. AllKeys upserts every key in State.
	Keys    []string
	AllKeys bool

	Plans             bool
	Routes            bool
	ConfigCredentials bool
	ResetSnapshots    []string
	UpstreamResets    []string

	NormalRequestEvents []RequestEvent
	RequestErrorEvents  []RequestErrorEvent
	RequestEventCutoff  time.Time
}

const maxPendingRequestRecords = 1000

func (c Changes) empty() bool {
	return len(c.Keys) == 0 && !c.AllKeys && !c.Plans && !c.Routes && !c.ConfigCredentials && len(c.ResetSnapshots) == 0 && len(c.UpstreamResets) == 0 &&
		len(c.NormalRequestEvents) == 0 && len(c.RequestErrorEvents) == 0 && c.RequestEventCutoff.IsZero()
}

func (c Changes) merge(next Changes) Changes {
	if c.empty() {
		return next.withBoundedRequestRecords()
	}
	if next.empty() {
		return c.withBoundedRequestRecords()
	}
	merged := Changes{
		AllKeys:             c.AllKeys || next.AllKeys,
		ResetSnapshots:      mergeChangeIDs(c.ResetSnapshots, next.ResetSnapshots),
		UpstreamResets:      mergeChangeIDs(c.UpstreamResets, next.UpstreamResets),
		Plans:               c.Plans || next.Plans,
		Routes:              c.Routes || next.Routes,
		ConfigCredentials:   c.ConfigCredentials || next.ConfigCredentials,
		NormalRequestEvents: append(append([]RequestEvent(nil), c.NormalRequestEvents...), next.NormalRequestEvents...),
		RequestErrorEvents:  append(append([]RequestErrorEvent(nil), c.RequestErrorEvents...), next.RequestErrorEvents...),
		RequestEventCutoff:  next.RequestEventCutoff,
	}
	if merged.RequestEventCutoff.Before(c.RequestEventCutoff) {
		merged.RequestEventCutoff = c.RequestEventCutoff
	}
	if merged.AllKeys {
		return merged.withBoundedRequestRecords()
	}
	merged.Keys = mergeChangeIDs(c.Keys, next.Keys)
	return merged.withBoundedRequestRecords()
}

func (c Changes) withBoundedRequestRecords() Changes {
	if len(c.NormalRequestEvents) > maxPendingRequestRecords {
		c.NormalRequestEvents = append([]RequestEvent(nil), c.NormalRequestEvents[len(c.NormalRequestEvents)-maxPendingRequestRecords:]...)
	}
	if len(c.RequestErrorEvents) > maxPendingRequestRecords {
		c.RequestErrorEvents = append([]RequestErrorEvent(nil), c.RequestErrorEvents[len(c.RequestErrorEvents)-maxPendingRequestRecords:]...)
	}
	return c
}

func mergeChangeIDs(first, second []string) []string {
	seen := make(map[string]bool, len(first)+len(second))
	var result []string
	for _, values := range [][]string{first, second} {
		for _, id := range values {
			if !seen[id] {
				seen[id] = true
				result = append(result, id)
			}
		}
	}
	return result
}
