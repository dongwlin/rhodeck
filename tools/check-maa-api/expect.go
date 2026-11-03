package main

import "sort"

const (
	ExpectPolicyRequired  = "required"
	ExpectPolicyOptional  = "optional"
	ExpectPolicyExcluded  = "excluded"
	ExpectPlatformAll     = "allPlatforms"
	ExpectPlatformWindows = "windowsOnly"
)

type Expectation struct {
	Policy   string
	Platform string
}

var policyExpectations = map[string]Expectation{
	"AsstSetUserDir":          {Policy: ExpectPolicyRequired, Platform: ExpectPlatformAll},
	"AsstLoadResource":        {Policy: ExpectPolicyRequired, Platform: ExpectPlatformAll},
	"AsstSetStaticOption":     {Policy: ExpectPolicyRequired, Platform: ExpectPlatformAll},
	"AsstCreateEx":            {Policy: ExpectPolicyRequired, Platform: ExpectPlatformAll},
	"AsstDestroy":             {Policy: ExpectPolicyRequired, Platform: ExpectPlatformAll},
	"AsstSetInstanceOption":   {Policy: ExpectPolicyRequired, Platform: ExpectPlatformAll},
	"AsstAppendTask":          {Policy: ExpectPolicyRequired, Platform: ExpectPlatformAll},
	"AsstSetTaskParams":       {Policy: ExpectPolicyRequired, Platform: ExpectPlatformAll},
	"AsstStart":               {Policy: ExpectPolicyRequired, Platform: ExpectPlatformAll},
	"AsstStop":                {Policy: ExpectPolicyRequired, Platform: ExpectPlatformAll},
	"AsstRunning":             {Policy: ExpectPolicyRequired, Platform: ExpectPlatformAll},
	"AsstConnected":           {Policy: ExpectPolicyRequired, Platform: ExpectPlatformAll},
	"AsstBackToHome":          {Policy: ExpectPolicyRequired, Platform: ExpectPlatformAll},
	"AsstAsyncConnect":        {Policy: ExpectPolicyRequired, Platform: ExpectPlatformAll},
	"AsstSetConnectionExtras": {Policy: ExpectPolicyRequired, Platform: ExpectPlatformAll},
	"AsstAsyncClick":          {Policy: ExpectPolicyRequired, Platform: ExpectPlatformAll},
	"AsstAsyncScreencap":      {Policy: ExpectPolicyRequired, Platform: ExpectPlatformAll},
	"AsstGetImage":            {Policy: ExpectPolicyRequired, Platform: ExpectPlatformAll},
	"AsstGetUUID":             {Policy: ExpectPolicyRequired, Platform: ExpectPlatformAll},
	"AsstGetTasksList":        {Policy: ExpectPolicyRequired, Platform: ExpectPlatformAll},
	"AsstGetNullSize":         {Policy: ExpectPolicyRequired, Platform: ExpectPlatformAll},
	"AsstGetVersion":          {Policy: ExpectPolicyRequired, Platform: ExpectPlatformAll},
	"AsstLog":                 {Policy: ExpectPolicyRequired, Platform: ExpectPlatformAll},
	"AsstCreate":              {Policy: ExpectPolicyOptional, Platform: ExpectPlatformAll},
	"AsstGetImageBgr":         {Policy: ExpectPolicyOptional, Platform: ExpectPlatformAll},
	"AsstGetMapLevelKey":      {Policy: ExpectPolicyOptional, Platform: ExpectPlatformAll},
	"AsstGetItemName":         {Policy: ExpectPolicyOptional, Platform: ExpectPlatformAll},
	"AsstAsyncAttachWindow":   {Policy: ExpectPolicyOptional, Platform: ExpectPlatformWindows},
	"AsstConnect":             {Policy: ExpectPolicyExcluded},
	"AsstAttachWindow":        {Policy: ExpectPolicyExcluded},
}

func sortedExpectationSymbols() []string {
	out := make([]string, 0, len(policyExpectations))
	for name := range policyExpectations {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

type ExpectSide struct {
	Policy   string
	Platform string
}

type ExpectEntry struct {
	Symbol   string
	Expected ExpectSide
	Actual   ExpectSide
}

type ExpectResult struct {
	Entries []ExpectEntry
}

func CompareExpectations(g *GoExtraction) (*ExpectResult, []Diagnostic) {
	fail := func(d Diagnostic) (*ExpectResult, []Diagnostic) { return nil, []Diagnostic{d} }
	if g == nil {
		return fail(Diagnostic{Category: CatPolicy, Symbol: "extraction",
			Expected: "validated go extraction", Actual: "nil",
			Message: "expectation comparison requires a validated go extraction"})
	}

	specByName := map[string]GoSymbolSpec{}
	for _, s := range g.Specs {
		if _, dup := specByName[s.Name]; !dup {
			specByName[s.Name] = s
		}
	}
	excludedNames := map[string]bool{}
	for _, e := range g.Exclusions {
		excludedNames[e.Name] = true
	}

	for _, s := range g.Specs {
		want, ok := policyExpectations[s.Name]
		if !ok {
			return fail(Diagnostic{Category: CatPolicy, File: s.File, Line: s.Line, Symbol: s.Name,
				Expected: "an entry in the independent binding expectation table",
				Actual:   "no frozen expectation for this symbol",
				Message:  "bound symbol is not frozen in the independent binding expectation table; unfrozen bindings are rejected"})
		}
		if want.Policy == ExpectPolicyExcluded {
			return fail(Diagnostic{Category: CatPolicy, File: s.File, Line: s.Line, Symbol: s.Name,
				Expected: "excluded (D and experimental exports must not be bound)",
				Actual:   "bound with Policy " + s.Policy + " and Platform " + s.Platform,
				Message:  "frozen expectation marks this symbol as excluded; rebinding a deprecated or experimental export fails"})
		}
		if s.Policy != want.Policy {
			return fail(Diagnostic{Category: CatPolicy, File: s.File, Line: s.Line, Symbol: s.Name,
				Expected: "Policy " + want.Policy, Actual: "Policy " + s.Policy,
				Message: "symbol policy does not match the frozen expectation"})
		}
		if s.Platform != want.Platform {
			return fail(Diagnostic{Category: CatPolicy, File: s.File, Line: s.Line, Symbol: s.Name,
				Expected: "Platform " + want.Platform, Actual: "Platform " + s.Platform,
				Message: "symbol platform does not match the frozen expectation"})
		}
	}

	for _, e := range g.Exclusions {
		want, ok := policyExpectations[e.Name]
		if ok && want.Policy != ExpectPolicyExcluded {
			return fail(Diagnostic{Category: CatPolicy, File: e.File, Line: e.Line, Symbol: e.Name,
				Expected: want.Policy + "/" + want.Platform + " binding without an exclusion",
				Actual:   "excluded",
				Message:  "frozen expectation requires this symbol to be bound; excluding an applicable R/O export fails"})
		}
	}

	for _, name := range sortedExpectationSymbols() {
		want := policyExpectations[name]
		if want.Policy == ExpectPolicyExcluded {
			continue
		}
		if _, bound := specByName[name]; bound {
			continue
		}
		return fail(Diagnostic{Category: CatPolicy, Symbol: name,
			Expected: want.Policy + "/" + want.Platform + " binding",
			Actual:   "unbound",
			Message:  "frozen expectation requires this symbol to be bound; a missing binding is rejected"})
	}

	res := &ExpectResult{Entries: make([]ExpectEntry, 0, len(policyExpectations))}
	for _, name := range sortedExpectationSymbols() {
		want := policyExpectations[name]
		var actual ExpectSide
		switch {
		case want.Policy == ExpectPolicyExcluded:
			actual = ExpectSide{Policy: ExpectPolicyExcluded}
			if !excludedNames[name] {
				actual.Policy = "unbound"
			}
		default:
			if s, bound := specByName[name]; bound {
				actual = ExpectSide{Policy: s.Policy, Platform: s.Platform}
			} else {
				actual = ExpectSide{Policy: "unbound"}
			}
		}
		res.Entries = append(res.Entries, ExpectEntry{Symbol: name, Expected: ExpectSide(want), Actual: actual})
	}
	return res, nil
}
