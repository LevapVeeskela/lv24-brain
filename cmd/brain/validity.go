package main

import (
	"flag"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// validityFlags are the temporal flags shared by every read command:
// --as-of selects an instant, --include-superseded (or its alias --all-time)
// keeps records whose validity ended.
type validityFlags struct {
	asOf              *string
	includeSuperseded *bool
	allTime           *bool
}

func addValidityFlags(fs *flag.FlagSet) validityFlags {
	return validityFlags{
		asOf:              fs.String("as-of", "", "read records valid at this instant: RFC3339 time or date (2026-11-01)"),
		includeSuperseded: fs.Bool("include-superseded", false, "include records whose validity ended"),
		allTime:           fs.Bool("all-time", false, "include every record regardless of validity (same as --include-superseded)"),
	}
}

// parse turns the flags into the instant and the include flag. An as-of instant
// selects the point in time, so it takes precedence over --include-superseded.
func (f validityFlags) parse() (time.Time, bool, error) {
	at, err := domain.ParseAsOf(*f.asOf)
	if err != nil {
		return time.Time{}, false, err
	}
	return at, *f.includeSuperseded || *f.allTime, nil
}

func (f validityFlags) include() bool {
	return *f.includeSuperseded || *f.allTime
}

// jsonValidity is the JSON form of domain.Validity. Times are RFC3339 or null;
// a null valid_to means the record is current.
type jsonValidity struct {
	ValidFrom    *string `json:"valid_from"`
	ValidTo      *string `json:"valid_to"`
	SupersededBy string  `json:"superseded_by,omitempty"`
	Supersedes   string  `json:"supersedes,omitempty"`
	Confidence   float64 `json:"confidence,omitempty"`
}

func toJSONValidity(validity domain.Validity, createdAt time.Time) jsonValidity {
	out := jsonValidity{
		ValidFrom:    jsonTime(validity.EffectiveFrom(createdAt)),
		SupersededBy: validity.SupersededBy,
		Supersedes:   validity.Supersedes,
		Confidence:   validity.Confidence,
	}
	if !validity.ValidTo.IsZero() {
		out.ValidTo = jsonTime(validity.ValidTo)
	}
	return out
}

func toJSONSummaryValidity(summary app.RecordSummary) jsonValidity {
	return jsonValidity{
		ValidFrom:    jsonTime(summary.ValidFrom),
		ValidTo:      jsonTime(summary.ValidTo),
		SupersededBy: summary.SupersededBy,
		Supersedes:   summary.Supersedes,
		Confidence:   summary.Confidence,
	}
}
