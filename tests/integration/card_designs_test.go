package integration

import (
	"net/http"
	"strconv"
	"testing"

	"github.com/tribe-africa/tribe-backend/tests/testenv"
)

// cardDesignRow is the card design the admin API answers with.
type cardDesignRow struct {
	ID        string  `json:"id"`
	Kind      string  `json:"kind"`
	Name      string  `json:"name"`
	Finish    string  `json:"finish"`
	HexColor  *string `json:"hex_color"`
	Pattern   *string `json:"pattern"`
	Enabled   bool    `json:"enabled"`
	SortOrder int     `json:"sort_order"`
	Version   int     `json:"version"`
}

// TestCardDesignAdminRules checks creating, editing, turning off and back
// on a card design: the entered id is kept, a taken id or look is 409
// DESIGN_EXISTS, every edit needs the current version (STALE_DATA when it
// is old), and fields of other kinds are stored empty.
func TestCardDesignAdminRules(test *testing.T) {
	environment := testenv.Start(test)
	admin := environment.SignInAdmin(test, "super@tribe.africa")
	expect := func(response *testenv.Response, status int, code string, what string) {
		test.Helper()
		if response.Status != status || (code != "" && response.Code != code) {
			test.Fatalf("%s = %d %s %s %v, want %d %s", what, response.Status, response.Code, response.Message, response.Errors, status, code)
		}
	}
	create := func(body map[string]any) *testenv.Response {
		return admin.Call(test, http.MethodPost, "/admin-api/card-designs", body, testenv.NewKey())
	}

	// The id typed in the form is the id stored; other kinds' fields are cleared.
	created := create(map[string]any{
		"id": "theme-aurora", "kind": "theme", "name": "Aurora", "finish": "aurora",
		"hex_color": "abcdef", "pattern": "dots", "enabled": true, "sort_order": 40,
	})
	expect(created, http.StatusCreated, "", "create theme-aurora")
	var design cardDesignRow
	created.Decode(test, &design)
	if design.ID != "theme-aurora" || design.Finish != "aurora" || design.HexColor != nil || design.Pattern != nil || design.Version != 1 {
		test.Fatalf("created design = %+v, want id theme-aurora, finish aurora, no hex, no pattern, version 1", design)
	}

	expect(create(map[string]any{"id": "theme-aurora", "kind": "theme", "name": "Again", "finish": "boreal", "enabled": true}),
		http.StatusConflict, "DESIGN_EXISTS", "creating a taken id")
	expect(create(map[string]any{"id": "theme-aurora-two", "kind": "theme", "name": "Aurora 2", "finish": "aurora", "enabled": true}),
		http.StatusConflict, "DESIGN_EXISTS", "creating a second theme with the same finish")
	expect(create(map[string]any{"id": "swatch-coral-two", "kind": "swatch", "name": "Coral 2", "hex_color": "ff7a5c", "enabled": true}),
		http.StatusConflict, "DESIGN_EXISTS", "creating a second swatch with the same hex")
	expect(create(map[string]any{"id": "pattern-dots-two", "kind": "pattern", "name": "Dots 2", "pattern": "dots", "enabled": true}),
		http.StatusConflict, "DESIGN_EXISTS", "creating a second pattern with the same name")

	// Validation per kind (422 with the field name).
	invalid := []struct {
		body  map[string]any
		field string
	}{
		{map[string]any{"id": "Theme_Bad", "kind": "theme", "name": "Bad", "finish": "bad"}, "id"},
		{map[string]any{"id": "theme-nofinish", "kind": "theme", "name": "No finish"}, "finish"},
		{map[string]any{"id": "swatch-bad", "kind": "swatch", "name": "Bad hex", "hex_color": "XYZ123"}, "hex_color"},
		{map[string]any{"id": "pattern-bad", "kind": "pattern", "name": "Bad", "pattern": "Bad Pattern"}, "pattern"},
		{map[string]any{"id": "pattern-late", "kind": "pattern", "name": "Late", "pattern": "late", "sort_order": 10001}, "sort_order"},
	}
	for _, item := range invalid {
		response := create(item.body)
		expect(response, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "creating "+item.body["id"].(string))
		if len(response.Errors[item.field]) == 0 {
			test.Errorf("creating %v: errors %v, want one for %s", item.body["id"], response.Errors, item.field)
		}
	}

	// A swatch's hex is stored upper case.
	var swatch cardDesignRow
	created = create(map[string]any{"id": "swatch-0A0B0C", "kind": "swatch", "name": "Ink", "hex_color": "0a0b0c", "finish": "ignored", "enabled": true})
	expect(created, http.StatusCreated, "", "create swatch")
	created.Decode(test, &swatch)
	if swatch.HexColor == nil || *swatch.HexColor != "0A0B0C" || swatch.Finish != "" || swatch.Pattern != nil {
		test.Fatalf("created swatch = %+v, want hex 0A0B0C and no finish or pattern", swatch)
	}

	// Edits need the current version.
	update := func(version int) *testenv.Response {
		return admin.Call(test, http.MethodPut, "/admin-api/card-designs/theme-aurora", map[string]any{
			"kind": "theme", "name": "Aurora Borealis", "finish": "aurora", "enabled": true, "sort_order": 41, "version": version,
		}, testenv.NewKey())
	}
	expect(update(0), http.StatusUnprocessableEntity, "VALIDATION_FAILED", "PUT with version 0")
	expect(update(2), http.StatusConflict, "STALE_DATA", "PUT with a stale version")
	saved := update(1)
	expect(saved, http.StatusOK, "", "PUT with the current version")
	saved.Decode(test, &design)
	if design.Version != 2 || design.Name != "Aurora Borealis" {
		test.Fatalf("saved design = %+v, want version 2 and the new name", design)
	}
	if missing := admin.Call(test, http.MethodPut, "/admin-api/card-designs/theme-nowhere", map[string]any{
		"kind": "theme", "name": "Nowhere", "finish": "nowhere", "version": 1,
	}, testenv.NewKey()); missing.Status != http.StatusNotFound {
		test.Fatalf("PUT on an unknown id = %d %s, want 404", missing.Status, missing.Code)
	}

	// Turning off and on again: DELETE ?version=, POST /enable {version}; both bump the version.
	disable := func(version string) *testenv.Response {
		path := "/admin-api/card-designs/theme-aurora"
		if version != "" {
			path += "?version=" + version
		}
		return admin.Call(test, http.MethodDelete, path, nil, testenv.NewKey())
	}
	expect(disable(""), http.StatusUnprocessableEntity, "VALIDATION_FAILED", "DELETE without a version")
	expect(disable("1"), http.StatusConflict, "STALE_DATA", "DELETE with a stale version")
	disabled := disable("2")
	expect(disabled, http.StatusOK, "", "DELETE with the current version")
	disabled.Decode(test, &design)
	if design.Enabled || design.Version != 3 {
		test.Fatalf("disabled design = %+v, want enabled false and version 3", design)
	}
	enable := func(version int) *testenv.Response {
		return admin.Call(test, http.MethodPost, "/admin-api/card-designs/theme-aurora/enable", map[string]int{"version": version}, testenv.NewKey())
	}
	expect(enable(2), http.StatusConflict, "STALE_DATA", "enable with a stale version")
	enabled := enable(3)
	expect(enabled, http.StatusOK, "", "enable with the current version")
	enabled.Decode(test, &design)
	if !design.Enabled || design.Version != 4 {
		test.Fatalf("enabled design = %+v, want enabled true and version 4", design)
	}

	// The admin list still shows disabled designs; sorted by sort_order then name.
	var catalog struct {
		Themes []cardDesignRow `json:"themes"`
	}
	admin.Call(test, http.MethodGet, "/admin-api/card-designs", nil, "").Decode(test, &catalog)
	for index := 1; index < len(catalog.Themes); index++ {
		previous, current := catalog.Themes[index-1], catalog.Themes[index]
		if previous.SortOrder > current.SortOrder || (previous.SortOrder == current.SortOrder && previous.Name > current.Name) {
			test.Errorf("themes out of order at %d: %+v before %+v", index, previous, current)
		}
	}
}

// TestCardPicksOnlyNeedTheirOwnDesigns checks which design rows a new card
// needs: a bare theme ("sunset") needs only the theme, so it still works
// when the plain pattern is turned off; "sunset:dots" needs the dots
// pattern too; and a card already wearing a turned-off design keeps it.
func TestCardPicksOnlyNeedTheirOwnDesigns(test *testing.T) {
	environment := testenv.Start(test)
	admin := environment.SignInAdmin(test, "super@tribe.africa")
	customer := environment.RegisterUser(test, "cardpicker")
	customer.TopUp(test, "USD", "100")

	turnOff := func(designID string) {
		test.Helper()
		var design cardDesignRow
		var catalog struct {
			Themes   []cardDesignRow `json:"themes"`
			Patterns []cardDesignRow `json:"patterns"`
		}
		admin.Call(test, http.MethodGet, "/admin-api/card-designs", nil, "").Decode(test, &catalog)
		for _, row := range append(catalog.Themes, catalog.Patterns...) {
			if row.ID == designID {
				design = row
			}
		}
		response := admin.Call(test, http.MethodDelete, "/admin-api/card-designs/"+designID+"?version="+strconv.Itoa(design.Version), nil, testenv.NewKey())
		if response.Status != http.StatusOK {
			test.Fatalf("turn off %s = %d %s %s", designID, response.Status, response.Code, response.Message)
		}
	}
	createCard := func(finish string) *testenv.Response {
		return customer.Call(test, http.MethodPost, "/api/mobile/v1/cards", map[string]any{
			"label": "Pick " + finish, "network": "visa", "design_finish": finish,
			"first_top_up": "5", "pay_with": "USD", "pin": testenv.DemoPin,
		}, testenv.NewKey())
	}

	turnOff("pattern-plain")
	bare := createCard("sunset")
	if !bare.Success {
		test.Fatalf("bare sunset with pattern-plain off = %d %s %s %v, want a new card", bare.Status, bare.Code, bare.Message, bare.Errors)
	}
	var created struct {
		Card struct {
			ID string `json:"id"`
		} `json:"card"`
	}
	bare.Decode(test, &created)
	card := created.Card

	if withWaves := createCard("sunset:waves"); !withWaves.Success {
		test.Fatalf("sunset:waves = %d %s %s, want a new card", withWaves.Status, withWaves.Code, withWaves.Message)
	}

	turnOff("pattern-dots")
	if withDots := createCard("sunset:dots"); withDots.Status != http.StatusUnprocessableEntity || len(withDots.Errors["design_finish"]) == 0 {
		test.Fatalf("sunset:dots with dots off = %d %s %v, want 422 on design_finish", withDots.Status, withDots.Code, withDots.Errors)
	}

	// Turning off the theme a card wears does not stop the card being
	// changed in other ways (re-sending its own finish is allowed), but
	// picking it fresh is refused.
	turnOff("theme-sunset")
	resend := customer.Call(test, http.MethodPatch, "/api/mobile/v1/cards/"+card.ID, map[string]any{
		"label": "Renamed", "design_finish": "sunset",
	}, testenv.NewKey())
	if !resend.Success {
		test.Fatalf("re-sending the card's own (now off) finish = %d %s %s %v, want success", resend.Status, resend.Code, resend.Message, resend.Errors)
	}
	if fresh := createCard("sunset"); fresh.Status != http.StatusUnprocessableEntity {
		test.Fatalf("a new card with the turned-off sunset theme = %d %s, want 422", fresh.Status, fresh.Code)
	}
	if cards := customer.Call(test, http.MethodGet, "/api/mobile/v1/cards", nil, ""); !cards.Success {
		test.Fatalf("listing cards that wear a turned-off design = %d %s", cards.Status, cards.Code)
	}
}
