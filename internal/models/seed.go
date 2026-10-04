package models

// SeedQuestion is a question payload used when writing a generated (or
// cloned) test to the database. The legacy code-level subject/test seeding
// is gone: subjects are managed in the database and every test is produced
// by the AI generator.
type SeedQuestion struct {
	Text       string
	Options    [4]string // A, B, C, D
	Correct    int       // 0=A, 1=B, 2=C, 3=D
	Topic      string
	Difficulty int
	// QualityChecked: the question already passed the quality audit (a
	// generated test after repairFlagged, or a clone of a checked question),
	// so it is stored with quality_checked_at = now() and the sweep skips it.
	QualityChecked bool
}
