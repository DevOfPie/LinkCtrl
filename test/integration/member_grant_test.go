//go:build integration

package integration

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// F163, end to end and through the browser, because the finding is about what
// the page says rather than about what the service does.
//
// The service is correct and stays correct: D31 wants the workspace-scoped
// membership to coexist with the organization-wide one, the COALESCE unique
// index in 00200_identity.sql is written to permit it, and refusing the
// redundant grant would take the useful *org editor plus workspace admin* case
// with it. What was wrong is that both outcomes produced one sentence —
// "Access granted. It adds to whatever they already had" — and for the grant
// that adds nothing that sentence asserts an effect that did not occur.
//
// Both directions in one test, because either alone passes on a page that
// always says the same thing, which is exactly the defect.
func TestTheGrantConfirmationSaysWhatTheGrantActuallyDid(t *testing.T) {
	f := newTeamFixture(t)
	orgAdmin := f.member(t, "admin@example.com", "admin")
	orgEditor := f.member(t, "editor@example.com", "editor")
	finance, err := f.team.CreateWorkspace(t.Context(), f.owner, "Finance")
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}

	srv, client := f.browser(t)
	post := func(path string, form url.Values) *http.Response {
		t.Helper()
		req, rerr := http.NewRequestWithContext(t.Context(), http.MethodPost,
			srv.URL+path, strings.NewReader(form.Encode()))
		if rerr != nil {
			t.Fatal(rerr)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		resp, derr := client.Do(req)
		if derr != nil {
			t.Fatal(derr)
		}
		return resp
	}
	get := func(path string) string {
		t.Helper()
		req, rerr := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+path, nil)
		if rerr != nil {
			t.Fatal(rerr)
		}
		resp, derr := client.Do(req)
		if derr != nil {
			t.Fatal(derr)
		}
		defer func() { _ = resp.Body.Close() }()
		body, berr := io.ReadAll(resp.Body)
		if berr != nil {
			t.Fatal(berr)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s = %d", path, resp.StatusCode)
		}
		return string(body)
	}
	grant := func(userID, role string) string {
		t.Helper()
		resp := post("/members", url.Values{
			"user_id":      {userID},
			"workspace_id": {finance.ID.String()},
			"role":         {role},
		})
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusSeeOther {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("granting %s = %d, want 303: %s", role, resp.StatusCode, body)
		}
		return resp.Header.Get("Location")
	}

	login := post("/login", url.Values{
		"email": {"owner@example.com"}, "password": {teamPassword},
	})
	_ = login.Body.Close()
	if login.StatusCode != http.StatusSeeOther {
		t.Fatalf("owner sign-in = %d, want 303", login.StatusCode)
	}

	// The grant that changes nothing. Editor's permissions are a strict subset
	// of the admin this person already holds across every workspace, so a row is
	// inserted, an audit entry is written, the list grows a line — and their
	// reach is what it was.
	if loc := grant(orgAdmin.UserID.String(), "editor"); loc != "/members?done=granted-nochange" {
		t.Errorf("granting editor to an organization-wide admin redirected to %q; "+
			"the confirmation is about what the grant did, and this one did nothing", loc)
	}
	page := get("/members?done=granted-nochange")
	if !strings.Contains(page, "it changes nothing they can do") {
		t.Error("the page does not say that the grant changed nothing")
	}
	if strings.Contains(page, "It adds to whatever they already had") {
		t.Error("the page still claims the grant added something")
	}

	// The grant D31 kept this path open for, in the same form, on the same
	// workspace, one rank apart. This one really does widen.
	if loc := grant(orgEditor.UserID.String(), "admin"); loc != "/members?done=granted" {
		t.Errorf("granting admin to an organization-wide editor redirected to %q, "+
			"want the ordinary confirmation", loc)
	}
	widened := get("/members?done=granted")
	if !strings.Contains(widened, "It adds to whatever they already had") {
		t.Error("a grant that widens somebody's reach is reported as changing nothing")
	}

	// Both memberships exist either way. The finding was never that the second
	// row should not be there.
	if n := f.count(t,
		`SELECT count(*) FROM memberships WHERE workspace_id = $1`, finance.ID); n != 2 {
		t.Errorf("%d workspace-scoped memberships in Finance, want 2", n)
	}
}

// The conditioned half of F163: the note beside the form answers for the
// selection, and the selection is what the three controls ask it again with.
//
// The server half of that round trip is what is asserted here — the same GET
// htmx issues on change, with the selects' values on the query string. The
// controls' own half is markup, and internal/ui asserts it there.
func TestTheGrantNoteAnswersForWhateverIsSelected(t *testing.T) {
	f := newTeamFixture(t)
	orgAdmin := f.member(t, "admin@example.com", "admin")
	orgEditor := f.member(t, "editor@example.com", "editor")
	finance, err := f.team.CreateWorkspace(t.Context(), f.owner, "Finance")
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}

	srv, client := f.browser(t)
	login, lerr := client.PostForm(srv.URL+"/login", url.Values{
		"email": {"owner@example.com"}, "password": {teamPassword},
	})
	if lerr != nil {
		t.Fatal(lerr)
	}
	_ = login.Body.Close()

	note := func(userID, role string) string {
		t.Helper()
		q := url.Values{
			"user_id": {userID}, "workspace_id": {finance.ID.String()}, "role": {role},
		}
		resp, gerr := client.Get(srv.URL + "/members?" + q.Encode())
		if gerr != nil {
			t.Fatal(gerr)
		}
		defer func() { _ = resp.Body.Close() }()
		body, berr := io.ReadAll(resp.Body)
		if berr != nil {
			t.Fatal(berr)
		}
		i := strings.Index(string(body), `id="grant-note"`)
		if i < 0 {
			t.Fatal("the page has no grant note, so a change has nowhere to land")
		}
		j := strings.Index(string(body)[i:], "</p>")
		return string(body)[i : i+j]
	}

	redundant := note(orgAdmin.UserID.String(), "viewer")
	if !strings.Contains(redundant, "already holds admin across every workspace") {
		t.Errorf("the note does not describe the selected person: %s", redundant)
	}
	if !strings.Contains(redundant, "bg-warn-soft") {
		t.Errorf("a grant that would change nothing is not drawn as one: %s", redundant)
	}

	widening := note(orgEditor.UserID.String(), "admin")
	if !strings.Contains(widening, "admin in Finance") {
		t.Errorf("the note does not describe the selected role and workspace: %s", widening)
	}
	if strings.Contains(widening, "bg-warn-soft") {
		t.Errorf("the case D31 kept this path open for is drawn as a warning: %s", widening)
	}
}

// F182's other half, asserted where a browser would meet it: the form a GET
// renders arrives on the lowest role the actor may assign, not the highest.
//
// Read out of the markup of the two controls rather than out of the handler,
// because the handler was never the bug — `Invites.Roles` and the D43 cap have
// always bound the list to the actor's own rank, and what a browser does with
// an unmarked list is what made the least deliberate path the most powerful
// one.
func TestBothAuthorityFormsArriveOnTheLowestRoleAnOwnerMayAssign(t *testing.T) {
	f := newTeamFixture(t)
	if _, err := f.team.CreateWorkspace(t.Context(), f.owner, "Finance"); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	// Somebody to grant to; the form is not drawn for an organization of one.
	f.member(t, "editor@example.com", "editor")

	srv, client := f.browser(t)
	login, err := client.PostForm(srv.URL+"/login", url.Values{
		"email": {"owner@example.com"}, "password": {teamPassword},
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = login.Body.Close()
	if login.StatusCode != http.StatusSeeOther {
		t.Fatalf("owner sign-in = %d, want 303", login.StatusCode)
	}

	for _, tc := range []struct{ path, control string }{
		{"/invites", `id="role"`},
		{"/members", `id="grant_role"`},
	} {
		t.Run(tc.path, func(t *testing.T) {
			resp, gerr := client.Get(srv.URL + tc.path)
			if gerr != nil {
				t.Fatal(gerr)
			}
			defer func() { _ = resp.Body.Close() }()
			raw, rerr := io.ReadAll(resp.Body)
			if rerr != nil {
				t.Fatal(rerr)
			}
			body := string(raw)
			i := strings.Index(body, tc.control)
			if i < 0 {
				t.Fatalf("no %s control on %s", tc.control, tc.path)
			}
			j := strings.Index(body[i:], "</select>")
			if j < 0 {
				t.Fatalf("the %s control is not a closed select", tc.control)
			}
			markup := body[i : i+j]

			// An owner is offered all four, so this is the finding exactly as it
			// was found: [Owner, Admin, Editor, Viewer], and a browser takes the
			// first of them unless the server says otherwise.
			for _, role := range []string{"owner", "admin", "editor", "viewer"} {
				if !strings.Contains(markup, `value="`+role+`"`) {
					t.Errorf("an owner is not offered %s: %s", role, markup)
				}
			}
			if !strings.Contains(markup, `<option value="viewer" selected>`) {
				t.Errorf("the form does not arrive on viewer: %s", markup)
			}
			if strings.Contains(markup, `<option value="owner" selected>`) {
				t.Errorf("the form arrives on owner, which is what F182 was: %s", markup)
			}
			if n := strings.Count(markup, " selected"); n != 1 {
				t.Errorf("%d of the four options are selected, want 1: %s", n, markup)
			}
		})
	}
}

// F182 on the **refusal** path of the invitation form, which is the arm that
// puts a posted value back.
//
// The GET arm above is the one the finding was written about, and it is fixed by
// choosing a default. This arm has a second way to lose the marker: it echoes
// what was posted, and a slug the form is not drawing marks no `<option>` at
// all. The select then arrives unmarked in exactly the state F182 describes, and
// a browser resolves that to the first entry — which is the top of a list
// ordered most powerful first.
//
// **An admin posting `role=owner` is the case, and it reaches here through the
// email.** D28's rank ceiling refuses owner outright with a 403, so the way that
// slug arrives on a re-rendered page is beside a field error on something else:
// `invite.Create` validates the address first, so a mistyped one returns a
// validation error while `role=owner` is still in the form values.
//
// The members page has always been right here — it echoes through `pickRole`,
// which is membership rather than non-emptiness — so this asserts the invitation
// form has caught up.
func TestARefusedInvitationDoesNotEchoARoleTheFormCannotOffer(t *testing.T) {
	f := newTeamFixture(t)
	f.member(t, "admin@example.com", "admin")

	srv, client := f.browser(t)
	login, err := client.PostForm(srv.URL+"/login", url.Values{
		"email": {"admin@example.com"}, "password": {teamPassword},
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = login.Body.Close()
	if login.StatusCode != http.StatusSeeOther {
		t.Fatalf("admin sign-in = %d, want 303", login.StatusCode)
	}

	resp, err := client.PostForm(srv.URL+"/invites", url.Values{
		"email": {"not-an-address"}, "role": {"owner"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("the refusal answered %d, want 422; this test needs the arm that "+
			"re-renders the form", resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	i := strings.Index(body, `id="role"`)
	if i < 0 {
		t.Fatal(`no id="role" control on the re-rendered invitation form`)
	}
	j := strings.Index(body[i:], "</select>")
	if j < 0 {
		t.Fatal(`the id="role" control is not a closed select`)
	}
	markup := body[i : i+j]

	if strings.Contains(markup, `value="owner"`) {
		t.Errorf("an admin is offered owner, so this test proves nothing about an "+
			"unofferable slug: %s", markup)
	}
	if n := strings.Count(markup, " selected"); n != 1 {
		t.Errorf("%d options are selected after a refusal that posted an unofferable "+
			"role, want 1; an unmarked select is what F182 was, and the browser "+
			"takes the first option — here, admin: %s", n, markup)
	}
	if !strings.Contains(markup, `<option value="viewer" selected>`) {
		t.Errorf("the re-rendered form does not arrive on viewer, which is the "+
			"weakest role an admin may offer: %s", markup)
	}
}
