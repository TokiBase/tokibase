package ruleguard

import (
	"sort"
	"strings"

	"github.com/tokibase/tokibase/kernel"
)

// Finding severities.
const (
	SeverityError = "error"
	SeverityInfo  = "info"
)

// Finding describes a public (empty string) rule.
type Finding struct {
	Collection string `json:"collection"`
	Rule       string `json:"rule"`
	Severity   string `json:"severity"`
	Message    string `json:"message"`
}

// Lint reports every non-system collection rule that is an empty string
// (public to anyone): "error" when not allowlisted, "info" when allowlisted.
// Rules that are null (superusers only) or non-empty expressions are ignored.
func Lint(app kernel.App, pol Policy) ([]Finding, error) {
	collections, err := app.FindAllCollections()
	if err != nil {
		return nil, err
	}
	return lintCollections(collections, pol), nil
}

type ruleRef struct {
	kind string
	rule *string
}

func collectionRules(c *kernel.Collection) []ruleRef {
	rules := []ruleRef{
		{KindList, c.ListRule},
		{KindView, c.ViewRule},
		{KindCreate, c.CreateRule},
		{KindUpdate, c.UpdateRule},
		{KindDelete, c.DeleteRule},
	}
	if c.IsAuth() {
		rules = append(rules, ruleRef{KindManage, c.ManageRule}, ruleRef{KindAuth, c.AuthRule})
	}
	return rules
}

func isPublic(rule *string) bool {
	return rule != nil && strings.TrimSpace(*rule) == ""
}

func lintCollections(collections []*kernel.Collection, pol Policy) []Finding {
	findings := []Finding{}
	for _, c := range collections {
		if c.System {
			continue
		}
		for _, r := range collectionRules(c) {
			if !isPublic(r.rule) {
				continue
			}
			f := Finding{Collection: c.Name, Rule: r.kind}
			if pol.Allowed(c.Name, r.kind) {
				f.Severity = SeverityInfo
				f.Message = "public " + r.kind + " rule is allowlisted"
			} else {
				f.Severity = SeverityError
				f.Message = "public " + r.kind + " rule (empty string) lets anyone access it; set a rule, use null for superusers only, or allowlist it with `rule allow " + c.Name + " " + r.kind + "`"
			}
			findings = append(findings, f)
		}
	}
	sort.SliceStable(findings, func(i, j int) bool { return findings[i].Collection < findings[j].Collection })
	return findings
}

// Errors returns only the error severity findings.
func Errors(findings []Finding) []Finding {
	out := []Finding{}
	for _, f := range findings {
		if f.Severity == SeverityError {
			out = append(out, f)
		}
	}
	return out
}
