package bot

import "fmt"

// NamedSource pairs a Source with the label stamped onto its items.
type NamedSource struct {
	Name   string
	Source Source
}

// MultiSource fans out to several sources and concatenates their items, tagging
// each with Meta["source"] = the sub-source's Name. A sub-source that errors is
// logged and skipped so one failure doesn't sink the whole digest — but if *every*
// sub-source fails it returns an error instead of an empty digest: "delivered
// nothing" and "there was nothing" must not look the same to the caller (scout,
// 2026-10-24: boot beat the network, all three sources skipped, and the job exited
// 0 with a week until its next window).
type MultiSource struct {
	Sources []NamedSource
}

func (m *MultiSource) Fetch() ([]Item, error) {
	var all []Item
	var failed []string
	for _, ns := range m.Sources {
		items, err := ns.Source.Fetch()
		if err != nil {
			fmt.Printf("multisource: skipping %s: %v\n", ns.Name, err)
			failed = append(failed, ns.Name)
			continue
		}
		for i := range items {
			if items[i].Meta == nil {
				items[i].Meta = map[string]string{}
			}
			items[i].Meta["source"] = ns.Name
		}
		all = append(all, items...)
	}
	if len(m.Sources) > 0 && len(failed) == len(m.Sources) {
		return nil, fmt.Errorf("every source failed (%v)", failed)
	}
	return all, nil
}
