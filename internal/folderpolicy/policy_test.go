package folderpolicy

import "testing"

func TestEquivalenceAndPrimaryAreExplicitOwnerChoices(t *testing.T) {
	folders := []string{"INBOX", "[Gmail]/Inbox", "Archive", "archive"}
	var empty Policy
	for _, remote := range folders {
		if key, excluded := empty.Membership(remote); key != "" || excluded {
			t.Fatalf("unselected folder %q was grouped/hidden", remote)
		}
	}
	p := Policy{Groups: []Group{{ID: "inbox-pair", Name: "Inbox", Primary: "[Gmail]/Inbox", Folders: folders[:2]}}}
	if err := p.Validate(folders); err != nil {
		t.Fatal(err)
	}
	for _, remote := range folders[:2] {
		if key, hidden := p.Membership(remote); key != "/groups/inbox-pair" || hidden {
			t.Fatalf("explicit member missing: %q %v", key, hidden)
		}
	}
	g, ok := p.GroupForKey("/groups/inbox-pair")
	if !ok || g.Primary != "[Gmail]/Inbox" {
		t.Fatal("primary replaced by inferred INBOX preference")
	}
	if key, hidden := p.Membership("Archive"); key != "" || hidden {
		t.Fatal("unrelated folder merged")
	}
	p = Policy{Excluded: []string{"Archive"}}
	if err := p.Validate(folders); err != nil {
		t.Fatal(err)
	}
	if _, hidden := p.Membership("archive"); hidden {
		t.Fatal("case-distinct remote folder silently hidden")
	}
}

func TestInvalidSelectionsCannotDiscardOrRedirectFolders(t *testing.T) {
	available := []string{"INBOX", "Alias", "Other"}
	for _, mode := range []string{"missing-primary", "outside-primary", "unknown-member", "overlap", "excluded-member", "duplicate-exclusion", "duplicate-inbox", "bad-id", "single-member"} {
		t.Run(mode, func(t *testing.T) {
			p := Policy{Groups: []Group{{ID: "pair", Name: "Mailbox", Primary: "INBOX", Folders: []string{"INBOX", "Alias"}}}}
			switch mode {
			case "missing-primary":
				p.Groups[0].Primary = ""
			case "outside-primary":
				p.Groups[0].Primary = "Other"
			case "unknown-member":
				p.Groups[0].Folders[1] = "Unknown"
			case "overlap":
				p.Groups = append(p.Groups, Group{ID: "other", Name: "Other", Primary: "Alias", Folders: []string{"Alias", "Other"}})
			case "excluded-member":
				p.Excluded = []string{"Alias"}
			case "duplicate-exclusion":
				p.Excluded = []string{"Other", "Other"}
			case "duplicate-inbox":
				p.Groups[0].Folders[1] = "inbox"
			case "bad-id":
				p.Groups[0].ID = "../folder"
			case "single-member":
				p.Groups[0].Folders = []string{"INBOX"}
			}
			if err := p.Validate(available); err == nil {
				t.Fatal("invalid choice accepted")
			}
		})
	}
}
