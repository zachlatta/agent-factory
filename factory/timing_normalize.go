package factory

const timingVersion = 1

// normalizeTiming uses one time zone for saved records and exported copies.
func normalizeTiming(t *CardTiming) {
	t.RequestedAt = t.RequestedAt.UTC()
	t.LandedAt = t.LandedAt.UTC()
	t.RepliedAt = t.RepliedAt.UTC()
	t.ReplyRetry = t.ReplyRetry.UTC()
	for i := range t.Events {
		t.Events[i].At = t.Events[i].At.UTC()
	}
	for i := range t.Rounds {
		t.Rounds[i].Start = t.Rounds[i].Start.UTC()
		t.Rounds[i].End = t.Rounds[i].End.UTC()
	}
	for i := range t.Mails {
		t.Mails[i].At = t.Mails[i].At.UTC()
	}
	t.Sessions = canonicalSessions(t.Sessions)
}
