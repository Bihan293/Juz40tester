package repositories

import (
	"encoding/json"
	"errors"
	"strings"
)

// R-8a: compact option_order. The column stays JSONB, but new rows store a
// JSON STRING of single-letter labels ("CADB") instead of the old JSON
// array (["C","A","D","B"]). Both formats are read; only the compact one is
// written, so attempts started before the upgrade keep working and no mass
// UPDATE of old rows is needed.

// encodeOptionOrder returns the JSON text stored in option_order. Labels
// that are not single characters (never produced by the bot) fall back to
// the legacy array so nothing is ever lost.
func encodeOptionOrder(order []string) (string, error) {
	compact := true
	for _, l := range order {
		if len(l) != 1 {
			compact = false
			break
		}
	}
	var v any = order
	if compact {
		v = strings.Join(order, "")
	}
	b, err := json.Marshal(v)
	return string(b), err
}

// decodeOptionOrder reads both the compact string and the legacy array.
func decodeOptionOrder(raw []byte) ([]string, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		out := make([]string, 0, len(s))
		for _, r := range s {
			out = append(out, string(r))
		}
		return out, nil
	}
	var arr []string
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil, errors.New("option_order: unsupported format")
	}
	return arr, nil
}

// DecodeOptionOrder is decodeOptionOrder for tests of other packages.
func DecodeOptionOrder(raw []byte) ([]string, error) { return decodeOptionOrder(raw) }
