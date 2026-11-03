package main

import (
	"path/filepath"
	"sort"
	"strings"
)

type LinkBound struct {
	Symbol   string
	Field    string
	Policy   string
	Platform string
}

type LinkResult struct {
	Bound     []LinkBound
	NonTarget []string
	Excluded  []string
}

func LinkCGo(c *CExtraction, g *GoExtraction, target Target) (*LinkResult, []Diagnostic) {
	fail := func(d Diagnostic) (*LinkResult, []Diagnostic) { return nil, []Diagnostic{d} }
	if c == nil || g == nil {
		return fail(Diagnostic{Category: CatRelation, Symbol: "extraction",
			Expected: "validated c and go extractions", Actual: "nil",
			Message: "linking requires validated c and go extraction results"})
	}

	fieldPos := map[string]GoField{}
	for _, f := range g.Fields {
		fieldPos[f.Name] = f
	}

	specByName := map[string]GoSymbolSpec{}
	boundFields := map[string]string{}
	for _, s := range g.Specs {
		if _, dup := specByName[s.Name]; dup {
			return fail(Diagnostic{Category: CatRelation, File: s.File, Line: s.Line, Symbol: s.Name,
				Expected: "unique symbolSpec entry", Actual: "duplicate entry",
				Message: "the same C symbol is bound more than once in symbolSpecs"})
		}
		specByName[s.Name] = s
		if _, ok := fieldPos[s.Field]; !ok {
			return fail(Diagnostic{Category: CatRelation, File: s.File, Line: s.Line, Symbol: s.Name,
				Expected: "Target references an existing api field", Actual: s.Field,
				Message: "symbolSpec Target points to a field that does not exist in the api struct"})
		}
		wantField := strings.TrimPrefix(s.Name, "Asst")
		if s.Field != wantField {
			return fail(Diagnostic{Category: CatRelation, File: s.File, Line: s.Line, Symbol: s.Name,
				Expected: "Target field " + wantField + " (symbol without the Asst prefix)",
				Actual:   s.Field,
				Message:  "symbolSpec Target field must equal the C symbol without the Asst prefix"})
		}
		if prev, dup := boundFields[s.Field]; dup {
			return fail(Diagnostic{Category: CatRelation, File: s.File, Line: s.Line, Symbol: s.Name,
				Expected: "each api field bound by exactly one entry", Actual: "field already bound by " + prev,
				Message: "two symbolSpec entries target the same api field"})
		}
		boundFields[s.Field] = s.Name
	}
	for _, f := range g.Fields {
		if _, ok := boundFields[f.Name]; !ok {
			return fail(Diagnostic{Category: CatRelation, File: f.File, Line: f.Line, Symbol: f.Name,
				Expected: "api field referenced by a symbolSpec entry", Actual: "no entry",
				Message: "api struct has a raw func field with no symbolSpec entry"})
		}
	}

	exclByName := map[string]GoExclusion{}
	for _, e := range g.Exclusions {
		if _, dup := exclByName[e.Name]; dup {
			return fail(Diagnostic{Category: CatRelation, File: e.File, Line: e.Line, Symbol: e.Name,
				Expected: "unique exclusion entry", Actual: "duplicate entry",
				Message: "the same C symbol is excluded more than once"})
		}
		exclByName[e.Name] = e
	}

	cByName := map[string]CFunction{}
	for i := range c.Functions {
		cByName[c.Functions[i].Name] = c.Functions[i]
	}
	applicable := map[string]bool{}
	for i := range c.Applicable {
		applicable[c.Applicable[i].Name] = true
	}

	for _, s := range g.Specs {
		if _, ok := cByName[s.Name]; !ok {
			return fail(Diagnostic{Category: CatRelation, File: s.File, Line: s.Line, Symbol: s.Name,
				Expected: "C export with the same name", Actual: "no C declaration",
				Message: "symbolSpec binds a symbol that has no C export in the locked input"})
		}
	}

	for _, s := range g.Specs {
		if _, excluded := exclByName[s.Name]; excluded {
			return fail(Diagnostic{Category: CatRelation, File: s.File, Line: s.Line, Symbol: s.Name,
				Expected: "excluded symbol without a binding", Actual: "also bound by symbolSpecs",
				Message: "an excluded export must not additionally appear in symbolSpecs"})
		}
	}

	for _, e := range g.Exclusions {
		fn, ok := cByName[e.Name]
		if !ok {
			return fail(Diagnostic{Category: CatRelation, File: e.File, Line: e.Line, Symbol: e.Name,
				Expected: "C declaration with the same name", Actual: "no C declaration",
				Message: "stale exclusion: no C export matches this exclusion entry"})
		}
		if got := filepath.Base(fn.Header); got != e.Header {
			return fail(Diagnostic{Category: CatRelation, File: e.File, Line: e.Line, Symbol: e.Name,
				Expected: "Header " + got + " (c declaration source header)", Actual: e.Header,
				Message: "exclusion Header does not match the C declaration source header"})
		}
	}

	for _, s := range g.Specs {
		fn := cByName[s.Name]
		switch {
		case fn.Platform == PlatformExtra && s.Policy != "optional":
			return fail(Diagnostic{Category: CatRelation, File: s.File, Line: s.Line, Symbol: s.Name,
				Expected: "Policy optional for an experimental export", Actual: "Policy " + s.Policy,
				Message: "experimental export must bind optional; a required binding rejects initialization on builds without the export"})
		case fn.Platform == PlatformExtra && s.Platform != "allPlatforms":
			return fail(Diagnostic{Category: CatRelation, File: s.File, Line: s.Line, Symbol: s.Name,
				Expected: "Platform allPlatforms for an experimental export", Actual: s.Platform,
				Message: "experimental export is option-conditional, not platform-conditional; binding it windows-only distorts the capability"})
		case fn.Platform == PlatformWindows && s.Platform != "windowsOnly":
			return fail(Diagnostic{Category: CatRelation, File: s.File, Line: s.Line, Symbol: s.Name,
				Expected: "Platform windowsOnly for a windows-conditional export", Actual: s.Platform,
				Message: "windows-conditional C export is bound with the wrong platform"})
		case fn.Platform == PlatformAll && s.Platform == "windowsOnly":
			return fail(Diagnostic{Category: CatRelation, File: s.File, Line: s.Line, Symbol: s.Name,
				Expected: "Platform allPlatforms for an unconditional export", Actual: s.Platform,
				Message: "unconditional C export is bound as windows-only"})
		}
	}

	res := &LinkResult{}
	for i := range c.Functions {
		fn := c.Functions[i]
		name := fn.Name
		s, hasSpec := specByName[name]
		_, excluded := exclByName[name]
		switch {
		case excluded:
			res.Excluded = append(res.Excluded, name)
		case applicable[name]:
			if !hasSpec {
				return fail(Diagnostic{Category: CatRelation, File: fn.Header, Line: fn.Line, Symbol: name,
					Expected: "symbolSpec entry binding this export", Actual: "no entry",
					Message: "applicable C export has no symbolSpec binding"})
			}
			res.Bound = append(res.Bound, LinkBound{Symbol: name, Field: s.Field,
				Policy: s.Policy, Platform: s.Platform})
		case fn.Platform == PlatformWindows:
			if !hasSpec || s.Platform != "windowsOnly" {
				return fail(Diagnostic{Category: CatRelation, File: fn.Header, Line: fn.Line, Symbol: name,
					Expected: "windowsOnly symbolSpec entry for the non-target platform export",
					Actual:   "missing or wrongly platformed entry",
					Message:  "windows export is neither excluded nor bound as windowsOnly"})
			}
			res.NonTarget = append(res.NonTarget, name)
		case fn.Platform == PlatformExtra:
			if !hasSpec {
				return fail(Diagnostic{Category: CatRelation, File: fn.Header, Line: fn.Line, Symbol: name,
					Expected: "symbolSpec binding or exclusion covering the experimental export", Actual: "no entry",
					Message: "experimental discovery is neither bound nor covered by a precise exclusion"})
			}
			res.Bound = append(res.Bound, LinkBound{Symbol: name, Field: s.Field,
				Policy: s.Policy, Platform: s.Platform})
		default:
			return fail(Diagnostic{Category: CatRelation, File: fn.Header, Line: fn.Line, Symbol: name,
				Expected: "classifiable export (all, windows or extra platform)", Actual: fn.Platform,
				Message: "c export carries an unknown platform label"})
		}
	}
	sort.Slice(res.Bound, func(i, j int) bool { return res.Bound[i].Symbol < res.Bound[j].Symbol })
	sort.Strings(res.NonTarget)
	sort.Strings(res.Excluded)
	return res, nil
}
