package fsstore

import (
	"fmt"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// taxonomyFrontmatter is the on-disk form of domain.Taxonomy. Only non-default
// values are written, so a record without classification keeps a clean
// frontmatter and every file written by an older engine stays readable.
type taxonomyFrontmatter struct {
	Subtype     string `yaml:"subtype,omitempty" json:"subtype,omitempty"`
	Scope       string `yaml:"scope,omitempty" json:"scope,omitempty"`
	TopicKey    string `yaml:"topic_key,omitempty" json:"topic_key,omitempty"`
	Occurrences int    `yaml:"occurrences,omitempty" json:"occurrences,omitempty"`
}

func taxonomyToFrontmatter(taxonomy domain.Taxonomy) taxonomyFrontmatter {
	taxonomy = taxonomy.Normalized()
	out := taxonomyFrontmatter{Subtype: taxonomy.Subtype, TopicKey: taxonomy.TopicKey}
	if taxonomy.Scope != domain.ScopeProject {
		out.Scope = string(taxonomy.Scope)
	}
	if taxonomy.Occurrences > 1 {
		out.Occurrences = taxonomy.Occurrences
	}
	return out
}

// taxonomyFromFrontmatter reads the optional classification. A topic key is
// normalized, so a hand-edited spelling still matches the same topic; an
// unknown scope or a negative occurrence count is an error, because silently
// defaulting it would hide a record from a scope filter.
func taxonomyFromFrontmatter(taxonomy taxonomyFrontmatter) (domain.Taxonomy, error) {
	scope := domain.RecordScope(taxonomy.Scope)
	if !scope.Valid() {
		return domain.Taxonomy{}, domain.NewValidationError("scope",
			fmt.Sprintf("unknown scope %q; use project, personal or global", taxonomy.Scope))
	}
	topicKey := ""
	if taxonomy.TopicKey != "" {
		normalized, err := domain.NormalizeTopicKey(taxonomy.TopicKey)
		if err != nil {
			return domain.Taxonomy{}, err
		}
		topicKey = normalized
	}
	if taxonomy.Occurrences < 0 {
		return domain.Taxonomy{}, domain.NewValidationError("occurrences", "must not be negative")
	}
	out := domain.Taxonomy{
		Subtype:     taxonomy.Subtype,
		Scope:       scope,
		TopicKey:    topicKey,
		Occurrences: taxonomy.Occurrences,
	}
	// The zero scope and occurrence count are not normalized here: an old file
	// without the fields keeps omitting them in JSON, while every filter and
	// index treats them as project/one.
	return out, nil
}
