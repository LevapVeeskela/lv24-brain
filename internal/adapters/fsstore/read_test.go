package fsstore

import "testing"

func TestParseHandoffBodyLegacyTemplate(t *testing.T) {
	body := "# Handoff 2026-11-05\n\n## Objective\n\nAudit done.\n\n## Completed\n\n## Next action\n\n## Evidence\n\n## Git log\n\n- 195333e1 Mark work\n"
	summary, gitContext, next := parseHandoffBody(body)
	if summary != "Audit done." || next != "" || gitContext != "- 195333e1 Mark work" {
		t.Fatalf("legacy handoff = %q %q %q", summary, gitContext, next)
	}
}

func TestParseHandoffBodyEngineSections(t *testing.T) {
	body := "# Handoff\n\n## Summary\n\nShipped.\n\n## Git context\n\nbranch=main\n\n## Next action\n\nRelease it\n"
	summary, gitContext, next := parseHandoffBody(body)
	if summary != "Shipped." || gitContext != "branch=main" || next != "Release it" {
		t.Fatalf("engine handoff = %q %q %q", summary, gitContext, next)
	}
}
