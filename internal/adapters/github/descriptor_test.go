package github

import (
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

func TestDescriptorHashesPersonalData(t *testing.T) {
	desc := Descriptor()
	if desc.Provider != Name {
		t.Fatalf("provider = %q", desc.Provider)
	}
	if !desc.PersonalData {
		t.Fatal("PersonalData must be true: commit authors and issue bodies carry e-mail addresses")
	}
	if desc.DefaultPublish != domain.PublishShared {
		t.Fatalf("default publish = %q, want shared", desc.DefaultPublish)
	}
}
