package fsstore

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/markdown"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// Proposals zone layout (contracts-v1, Phase 4.1):
//
//	proposals/.gitignore                         "*", so nothing is committed
//	proposals/<project>/<proposal-id>.md          one candidate record
//	proposals/<project>/<proposal-id>.md.lock     cross-process write lock
//
// proposals/ is machine-local derived data. CurrentRevision excludes the
// directory (see isExcludedRootDir), so digesting external items never changes
// the curated revision or git status. A proposal file is YAML frontmatter plus
// a human-readable Markdown body rendered from the same fields; the
// frontmatter is canonical. The file name is the deterministic proposal id.
const (
	proposalsDir        = "proposals"
	proposalFileExt     = ".md"
	proposalIgnoreFile  = ".gitignore"
	proposalKindStorage = "proposal"
)

// proposalIgnore is written once at proposals/.gitignore: proposals are
// machine-local and must never be committed by brain sync.
const proposalIgnore = "# lv24-brain: proposals are machine-local derived data; nothing in this directory is committed.\n*\n"

type proposalFrontmatter struct {
	markdown.Frontmatter `yaml:",inline"`
	ID                   string                        `yaml:"id,omitempty"`
	ProposalKind         domain.ProposalKind           `yaml:"proposal_kind,omitempty"`
	Rule                 string                        `yaml:"rule,omitempty"`
	Confidence           float64                       `yaml:"confidence,omitempty"`
	ConnectionID         string                        `yaml:"connection_id,omitempty"`
	ExternalID           string                        `yaml:"external_id,omitempty"`
	Title                string                        `yaml:"title,omitempty"`
	Body                 string                        `yaml:"body,omitempty"`
	Sections             []proposalSectionFrontmatter  `yaml:"sections,omitempty"`
	Citations            []proposalCitationFrontmatter `yaml:"citations,omitempty"`
	PayloadHash          string                        `yaml:"payload_hash,omitempty"`
	Status               domain.ProposalStatus         `yaml:"status,omitempty"`
	CreatedAt            time.Time                     `yaml:"created_at,omitempty"`
	PromotedRecordID     string                        `yaml:"promoted_record_id,omitempty"`
	PromotedAt           *time.Time                    `yaml:"promoted_at,omitempty"`
}

type proposalSectionFrontmatter struct {
	Heading string `yaml:"heading"`
	Body    string `yaml:"body,omitempty"`
}

type proposalCitationFrontmatter struct {
	ConnectionID string `yaml:"connection_id,omitempty"`
	ItemKey      string `yaml:"item_key,omitempty"`
	ExternalID   string `yaml:"external_id,omitempty"`
	URL          string `yaml:"url,omitempty"`
	Title        string `yaml:"title,omitempty"`
	ObservedAt   string `yaml:"observed_at,omitempty"`
}

var _ ports.ProposalStore = (*Store)(nil)
var _ ports.ProposalProblemReporter = (*Store)(nil)

// Put creates or refreshes one pending proposal. See ports.ProposalStore.
func (s *Store) Put(ctx context.Context, proposal domain.Proposal) (domain.Proposal, error) {
	if err := contextError(ctx); err != nil {
		return domain.Proposal{}, err
	}
	if proposal.SchemaVersion == 0 {
		proposal.SchemaVersion = domain.ProposalSchemaVersion
	}
	if proposal.Status == "" {
		proposal.Status = domain.ProposalPending
	}
	if proposal.Status != domain.ProposalPending {
		return domain.Proposal{}, domain.NewValidationError("proposal.status", "Put only writes pending proposals; use SetStatus for promotion or dismissal")
	}
	if proposal.ID == "" {
		proposal.ID = domain.ProposalID(proposal.ProjectID, proposal.ConnectionID, proposal.ExternalID)
	}
	if proposal.PayloadHash == "" {
		proposal.PayloadHash = proposal.ComputePayloadHash()
	}
	if err := proposal.Validate(); err != nil {
		return domain.Proposal{}, err
	}
	if err := s.requireProject(proposal.ProjectID); err != nil {
		return domain.Proposal{}, err
	}
	if err := s.ensureProposalsIgnore(); err != nil {
		return domain.Proposal{}, err
	}
	path, err := s.proposalPath(proposal.ProjectID, proposal.ID)
	if err != nil {
		return domain.Proposal{}, err
	}
	now := s.nowUTC()
	stored := proposal
	err = withFileLock(ctx, path, func() error {
		existing, readErr := readProposalFile(path)
		switch {
		case readErr == nil:
			if err := checkProjectMatch(path, existing.ProjectID, proposal.ProjectID); err != nil {
				return err
			}
			if existing.Status.Terminal() {
				// A promoted or dismissed proposal is terminal: a digest
				// rerun must never resurrect or overwrite it.
				stored = existing
				return nil
			}
			if existing.PayloadHash == proposal.PayloadHash {
				// Idempotent rerun: keep timestamps and status exactly as
				// stored (including a stale marker).
				stored = existing
				return nil
			}
			proposal.CreatedAt, proposal.UpdatedAt = proposalTimes(existing.CreatedAt, now)
			proposal.PromotedRecordID = ""
			proposal.PromotedAt = time.Time{}
			stored = proposal
			return writeProposalFile(path, proposal)
		case errors.Is(readErr, fs.ErrNotExist):
			proposal.CreatedAt, proposal.UpdatedAt = proposalTimes(proposal.CreatedAt, now)
			stored = proposal
			return writeProposalFile(path, proposal)
		default:
			// A damaged derived file is repaired by the fresh content. It is
			// reported by ProposalProblems until a digest replaces it.
			proposal.CreatedAt, proposal.UpdatedAt = proposalTimes(proposal.CreatedAt, now)
			stored = proposal
			return writeProposalFile(path, proposal)
		}
	})
	return stored, err
}

// Get returns one proposal or domain.ErrNotFound.
func (s *Store) Get(ctx context.Context, projectID, id string) (domain.Proposal, error) {
	if err := contextError(ctx); err != nil {
		return domain.Proposal{}, err
	}
	if err := domain.ValidateProposalID("proposal.id", id); err != nil {
		return domain.Proposal{}, err
	}
	if err := s.requireProject(projectID); err != nil {
		return domain.Proposal{}, err
	}
	path, err := s.proposalPath(projectID, id)
	if err != nil {
		return domain.Proposal{}, err
	}
	proposal, err := readProposalFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return domain.Proposal{}, notFound("proposal", id)
	}
	if err != nil {
		return domain.Proposal{}, err
	}
	if err := checkProjectMatch(path, proposal.ProjectID, projectID); err != nil {
		return domain.Proposal{}, err
	}
	return proposal, nil
}

// List returns proposals of one project newest updated first.
func (s *Store) List(ctx context.Context, projectID string, opts ports.ProposalListOptions) ([]domain.Proposal, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	if opts.Limit < 0 {
		return nil, domain.NewValidationError("proposals.limit", "must not be negative")
	}
	for _, status := range opts.Statuses {
		if !status.Valid() {
			return nil, domain.NewValidationError("proposals.statuses", fmt.Sprintf("unknown status %q", status))
		}
	}
	if opts.Kind != "" && !opts.Kind.Valid() {
		return nil, domain.NewValidationError("proposals.kind", fmt.Sprintf("unknown kind %q", opts.Kind))
	}
	if opts.ConnectionID != "" {
		if err := domain.ValidateConnectionID("proposals.connectionId", opts.ConnectionID); err != nil {
			return nil, err
		}
	}
	if err := s.requireProject(projectID); err != nil {
		return nil, err
	}
	dir, err := s.proposalProjectDir(projectID)
	if err != nil {
		return nil, err
	}
	paths, err := proposalPaths(dir)
	if err != nil {
		return nil, err
	}
	proposals := make([]domain.Proposal, 0, len(paths))
	for _, path := range paths {
		if err := contextError(ctx); err != nil {
			return nil, err
		}
		proposal, err := readProposalFile(path)
		if err != nil {
			// Listing skips damaged files; ProposalProblems reports them.
			continue
		}
		if proposal.ProjectID != projectID || !proposalMatches(proposal, opts) {
			continue
		}
		proposals = append(proposals, proposal)
	}
	sort.SliceStable(proposals, func(i, j int) bool {
		if !proposals[i].UpdatedAt.Equal(proposals[j].UpdatedAt) {
			return proposals[i].UpdatedAt.After(proposals[j].UpdatedAt)
		}
		return proposals[i].ID < proposals[j].ID
	})
	if opts.Limit > 0 && len(proposals) > opts.Limit {
		proposals = proposals[:opts.Limit]
	}
	return proposals, nil
}

// SetStatus moves a proposal to status. Terminal statuses are frozen: another
// status is domain.ErrConflict. Repeating a terminal status is idempotent and,
// for promoted, repairs the promoted record id.
func (s *Store) SetStatus(ctx context.Context, projectID, id string, status domain.ProposalStatus, recordID string) (domain.Proposal, error) {
	if err := contextError(ctx); err != nil {
		return domain.Proposal{}, err
	}
	if !status.Valid() {
		return domain.Proposal{}, domain.NewValidationError("proposal.status", fmt.Sprintf("unknown status %q", status))
	}
	if err := domain.ValidateProposalID("proposal.id", id); err != nil {
		return domain.Proposal{}, err
	}
	if err := s.requireProject(projectID); err != nil {
		return domain.Proposal{}, err
	}
	path, err := s.proposalPath(projectID, id)
	if err != nil {
		return domain.Proposal{}, err
	}
	now := s.nowUTC()
	var stored domain.Proposal
	err = withFileLock(ctx, path, func() error {
		proposal, err := readProposalFile(path)
		if errors.Is(err, fs.ErrNotExist) {
			return notFound("proposal", id)
		}
		if err != nil {
			return err
		}
		if err := checkProjectMatch(path, proposal.ProjectID, projectID); err != nil {
			return err
		}
		if proposal.Status.Terminal() {
			if proposal.Status != status {
				return fmt.Errorf("fsstore: proposal %q is %s: %w", id, proposal.Status, domain.ErrConflict)
			}
			if proposal.Status == domain.ProposalPromoted && recordID != "" && recordID != proposal.PromotedRecordID {
				proposal.PromotedRecordID = recordID
				if proposal.PromotedAt.IsZero() {
					proposal.PromotedAt = now
				}
				proposal.CreatedAt, proposal.UpdatedAt = proposalTimes(proposal.CreatedAt, now)
				if err := proposal.Validate(); err != nil {
					return err
				}
				if err := writeProposalFile(path, proposal); err != nil {
					return err
				}
			}
			stored = proposal
			return nil
		}
		if status == domain.ProposalPromoted {
			if recordID == "" {
				return domain.NewValidationError("proposal.promotedRecordId", "is required for a promoted proposal")
			}
			proposal.PromotedRecordID = recordID
			if proposal.PromotedAt.IsZero() {
				proposal.PromotedAt = now
			}
		} else {
			if recordID != "" {
				return domain.NewValidationError("proposal.promotedRecordId", "is only valid for a promoted proposal")
			}
			proposal.PromotedRecordID = ""
			proposal.PromotedAt = time.Time{}
		}
		if proposal.Status == status && proposal.PromotedRecordID == recordID {
			stored = proposal
			return nil
		}
		proposal.Status = status
		proposal.CreatedAt, proposal.UpdatedAt = proposalTimes(proposal.CreatedAt, now)
		if err := proposal.Validate(); err != nil {
			return err
		}
		stored = proposal
		return writeProposalFile(path, proposal)
	})
	return stored, err
}

// ProposalProblems reports proposal files that List skips because they are
// unreadable, malformed or belong to another project.
func (s *Store) ProposalProblems(ctx context.Context, projectID string) ([]ports.RecordProblem, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	if err := s.requireProject(projectID); err != nil {
		return nil, err
	}
	dir, err := s.proposalProjectDir(projectID)
	if err != nil {
		return nil, err
	}
	paths, err := proposalPaths(dir)
	if err != nil {
		return nil, err
	}
	problems := make([]ports.RecordProblem, 0)
	for _, path := range paths {
		proposal, err := readProposalFile(path)
		if err == nil && proposal.ProjectID == projectID {
			continue
		}
		if err == nil {
			err = fmt.Errorf("fsstore: proposal %s belongs to project %q, not %q", path, proposal.ProjectID, projectID)
		}
		problems = append(problems, ports.RecordProblem{Path: path, Kind: proposalKindStorage, Reason: err.Error()})
	}
	return problems, nil
}

// proposalTimes keeps created_at authoritative while making sure updated_at is
// never earlier than it (a caller may supply a creation time slightly ahead of
// a coarse store clock).
func proposalTimes(created, now time.Time) (time.Time, time.Time) {
	created = recordCreatedAt(created, now)
	updated := now.UTC()
	if updated.Before(created) {
		updated = created
	}
	return created, updated
}

func proposalMatches(proposal domain.Proposal, opts ports.ProposalListOptions) bool {
	if len(opts.Statuses) > 0 {
		matched := false
		for _, status := range opts.Statuses {
			if proposal.Status == status {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	if opts.Kind != "" && proposal.Kind != opts.Kind {
		return false
	}
	if opts.ConnectionID != "" && proposal.ConnectionID != opts.ConnectionID {
		return false
	}
	return opts.Rule == "" || proposal.Rule == opts.Rule
}

func (s *Store) ensureProposalsIgnore() error {
	dir := filepath.Join(s.root, proposalsDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("fsstore: create proposals directory: %w", err)
	}
	path := filepath.Join(dir, proposalIgnoreFile)
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("fsstore: check %s: %w", path, err)
	}
	if err := writeFileAtomic(path, []byte(proposalIgnore)); err != nil {
		return fmt.Errorf("fsstore: write proposals ignore: %w", err)
	}
	return nil
}

func (s *Store) proposalProjectDir(projectID string) (string, error) {
	if err := validateProjectID(projectID); err != nil {
		return "", err
	}
	return s.ensureInsideRoot(filepath.Join(s.root, proposalsDir, projectID))
}

func (s *Store) proposalPath(projectID, id string) (string, error) {
	if err := domain.ValidateProposalID("proposal.id", id); err != nil {
		return "", err
	}
	dir, err := s.proposalProjectDir(projectID)
	if err != nil {
		return "", err
	}
	path, err := s.ensureInsideRoot(filepath.Join(dir, id+proposalFileExt))
	if err != nil {
		return "", err
	}
	return path, nil
}

// proposalPaths lists sorted proposal Markdown files in dir. Hidden files
// (including .gitignore) and lock files never match.
func proposalPaths(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("fsstore: list proposals: %w", err)
	}
	paths := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasPrefix(name, ".") || !strings.HasSuffix(strings.ToLower(name), proposalFileExt) {
			continue
		}
		paths = append(paths, filepath.Join(dir, name))
	}
	sort.Strings(paths)
	return paths, nil
}

func writeProposalFile(path string, proposal domain.Proposal) error {
	raw, err := encodeProposal(proposal)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(path, raw); err != nil {
		return fmt.Errorf("fsstore: write proposal %q: %w", proposal.ID, err)
	}
	return nil
}

func encodeProposal(proposal domain.Proposal) ([]byte, error) {
	front := proposalFrontmatter{
		Frontmatter: markdown.Frontmatter{
			ProjectID:     proposal.ProjectID,
			Kind:          proposalKindStorage,
			UpdatedAt:     proposal.UpdatedAt,
			SchemaVersion: proposal.SchemaVersion,
		},
		ID:               proposal.ID,
		ProposalKind:     proposal.Kind,
		Rule:             proposal.Rule,
		Confidence:       proposal.Confidence,
		ConnectionID:     proposal.ConnectionID,
		ExternalID:       proposal.ExternalID,
		Title:            proposal.Title,
		Body:             proposal.Body,
		PayloadHash:      proposal.PayloadHash,
		Status:           proposal.Status,
		CreatedAt:        proposal.CreatedAt,
		PromotedRecordID: proposal.PromotedRecordID,
	}
	if !proposal.PromotedAt.IsZero() {
		at := proposal.PromotedAt.UTC()
		front.PromotedAt = &at
	}
	for _, section := range proposal.Sections {
		front.Sections = append(front.Sections, proposalSectionFrontmatter{
			Heading: section.Heading,
			Body:    section.Body,
		})
	}
	for _, citation := range proposal.Citations {
		front.Citations = append(front.Citations, proposalCitationFrontmatter{
			ConnectionID: citation.ConnectionID,
			ItemKey:      citation.ItemKey,
			ExternalID:   citation.ExternalID,
			URL:          citation.URL,
			Title:        citation.Title,
			ObservedAt:   formatItemTime(citation.ObservedAt),
		})
	}
	// Force double-quoted scalar style for every string. Body and section
	// text may contain a literal "---" line; a YAML block scalar would indent
	// it, but markdown.Parse trims each line and would mistake it for the
	// closing frontmatter delimiter.
	var node yaml.Node
	if err := node.Encode(front); err != nil {
		return nil, fmt.Errorf("fsstore: encode proposal %q: %w", proposal.ID, err)
	}
	quoteStringValues(&node)
	raw, err := markdown.Render(&node, renderProposalBody(proposal))
	if err != nil {
		return nil, fmt.Errorf("fsstore: render proposal %q: %w", proposal.ID, err)
	}
	return raw, nil
}

func renderProposalBody(proposal domain.Proposal) string {
	var out strings.Builder
	fmt.Fprintf(&out, "# %s\n", singleLine(proposal.Title))
	if body := strings.TrimSpace(proposal.Body); body != "" {
		out.WriteString("\n")
		out.WriteString(body)
		out.WriteString("\n")
	}
	for _, section := range proposal.Sections {
		fmt.Fprintf(&out, "\n## %s\n\n", singleLine(section.Heading))
		out.WriteString(strings.TrimSpace(section.Body))
		out.WriteString("\n")
	}
	return out.String()
}

func readProposalFile(path string) (domain.Proposal, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return domain.Proposal{}, err
	}
	var front proposalFrontmatter
	if _, err := markdown.Parse(raw, &front); err != nil {
		return domain.Proposal{}, fmt.Errorf("fsstore: proposal %s: %w", path, err)
	}
	if err := validateRecordFrontmatter(path, front.Frontmatter, proposalKindStorage); err != nil {
		return domain.Proposal{}, err
	}
	if front.SchemaVersion == 0 {
		return domain.Proposal{}, domain.NewValidationError("schema_version", path+": is required")
	}
	proposal := domain.Proposal{
		SchemaVersion:    front.SchemaVersion,
		ProjectID:        front.ProjectID,
		ID:               front.ID,
		Kind:             front.ProposalKind,
		Rule:             front.Rule,
		Confidence:       front.Confidence,
		ConnectionID:     front.ConnectionID,
		ExternalID:       front.ExternalID,
		Title:            front.Title,
		Body:             front.Body,
		PayloadHash:      front.PayloadHash,
		Status:           front.Status,
		CreatedAt:        front.CreatedAt.UTC(),
		UpdatedAt:        front.UpdatedAt.UTC(),
		PromotedRecordID: front.PromotedRecordID,
	}
	if front.PromotedAt != nil {
		proposal.PromotedAt = front.PromotedAt.UTC()
	}
	for _, section := range front.Sections {
		proposal.Sections = append(proposal.Sections, domain.ProposalSection{
			Heading: section.Heading,
			Body:    section.Body,
		})
	}
	for _, citation := range front.Citations {
		at, err := parseItemTime(citation.ObservedAt)
		if err != nil {
			return domain.Proposal{}, fmt.Errorf("fsstore: proposal %s: citation observed_at: %w", path, err)
		}
		proposal.Citations = append(proposal.Citations, domain.ProposalCitation{
			ConnectionID: citation.ConnectionID,
			ItemKey:      citation.ItemKey,
			ExternalID:   citation.ExternalID,
			URL:          citation.URL,
			Title:        citation.Title,
			ObservedAt:   at,
		})
	}
	if proposal.ID == "" {
		return domain.Proposal{}, domain.NewValidationError("id", path+": is required")
	}
	if filepath.Base(path) != proposal.ID+proposalFileExt {
		return domain.Proposal{}, domain.NewValidationError("id",
			fmt.Sprintf("%s: file name does not match proposal id %q", path, proposal.ID))
	}
	if err := proposal.Validate(); err != nil {
		return domain.Proposal{}, fmt.Errorf("fsstore: proposal %s: %w", path, err)
	}
	return proposal, nil
}
