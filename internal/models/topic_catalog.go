package models

// TopicCatalog is the topic reference of one subject (B1): canonical topics
// (subject_topics) and every known normalised spelling (topic_aliases).
type TopicCatalog struct {
	SubjectID int64
	// Titles are the canonical topic titles, most used first.
	Titles []string
	// Aliases maps a normalised spelling (NormalizeTopic) to its topic_key.
	Aliases map[string]string
}

// Resolve maps a raw topic title to its topic_key ("" , false when the topic
// is not in the catalog).
func (c *TopicCatalog) Resolve(raw string) (string, bool) {
	if c == nil {
		return "", false
	}
	n := NormalizeTopic(raw)
	if n == "" {
		return "", false
	}
	k, ok := c.Aliases[n]
	return k, ok
}
