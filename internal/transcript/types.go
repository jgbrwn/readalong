package transcript

type Word struct {
	Text       string  `json:"t"`
	StartMS    int64   `json:"s"`
	EndMS      int64   `json:"e"`
	Confidence float64 `json:"c,omitempty"`
}
type Sentence struct {
	ID          string `json:"id"`
	ParagraphID string `json:"paragraph_id,omitempty"`
	StartMS     int64  `json:"start_ms"`
	EndMS       int64  `json:"end_ms"`
	Words       []Word `json:"words"`
}
type Document struct {
	Version    int        `json:"version"`
	DurationMS int64      `json:"duration_ms"`
	Sentences  []Sentence `json:"sentences"`
}
