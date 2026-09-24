package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/slbdotdev/slbh/internal/config"
	"github.com/slbdotdev/slbh/internal/provider"
	"github.com/slbdotdev/slbh/internal/seam"
)

type modelTreeNode struct {
	provider int
	model    int
	branch   bool
	save     bool
}

func (m *Model) openModelMenu() tea.Cmd {
	if m.modelExpanded == nil {
		m.modelExpanded = make(map[string]bool)
	}
	cfg := m.runtime.Config()
	m.modelSeat, m.modelSubagent, m.modelLeaf = "", "", ""
	if cfg.ModelApproved(cfg.SeatModel) {
		m.modelSeat = cfg.SeatModel
	}
	if cfg.ModelApproved(cfg.SubagentModel) {
		m.modelSubagent = cfg.SubagentModel
	}
	leaf := cfg.LeafModel
	if leaf == "" {
		leaf = cfg.SubagentModel
	}
	if cfg.ModelApproved(leaf) {
		m.modelLeaf = leaf
	}
	m.modelsOpen = true
	m.modelsLoading = true
	m.modelNotice = "loading provider catalogs…"
	m.modelNoticeErr = false
	m.modelNoticeOK = false
	m.input.Blur()
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		catalog, err := provider.DiscoverCatalog(ctx, m.runtime.Config().Policy)
		return modelCatalogMsg{catalog: catalog, err: err}
	}
}

func (m *Model) updateModelMenu(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if msg.Code == tea.KeyEsc {
		if m.modelSubmenu {
			m.modelSubmenu = false
			return m, nil
		}
		m.saveAndCloseModelMenu()
		return m, nil
	}
	if m.modelSubmenu {
		return m.updateModelSubmenu(msg)
	}
	if m.modelsLoading {
		return m, nil
	}
	nodes := m.modelNodes()
	if len(nodes) > 0 && m.modelCursor >= len(nodes) {
		m.modelCursor = len(nodes) - 1
	}
	switch msg.Code {
	case tea.KeyUp:
		if m.modelCursor > 0 {
			m.modelCursor--
		}
	case tea.KeyDown:
		if m.modelCursor < len(nodes)-1 {
			m.modelCursor++
		}
	case tea.KeyLeft:
		if len(nodes) > 0 && !nodes[m.modelCursor].save {
			m.modelExpanded[m.modelCatalog[nodes[m.modelCursor].provider].Name] = false
		}
	case tea.KeyRight:
		if len(nodes) > 0 && nodes[m.modelCursor].branch {
			m.modelExpanded[m.modelCatalog[nodes[m.modelCursor].provider].Name] = true
		}
	case tea.KeyEnter:
		if len(nodes) > 0 {
			node := nodes[m.modelCursor]
			switch {
			case node.save:
				m.saveAndCloseModelMenu()
			case node.branch:
				name := m.modelCatalog[node.provider].Name
				m.modelExpanded[name] = !m.modelExpanded[name]
			default:
				m.modelSubmenu = true
				m.modelSubnode = node
				m.modelSubcursor = 0
			}
		}
	case 'r', 'R', 's', 'S', 'l', 'L':
		if len(nodes) > 0 && !nodes[m.modelCursor].branch && !nodes[m.modelCursor].save {
			slot := "seat"
			switch strings.ToLower(string(msg.Code)) {
			case "s":
				slot = "subagent"
			case "l":
				slot = "leaf"
			}
			m.assignModel(nodes[m.modelCursor], slot)
		}
	case 'p', 'P':
		m.authorLocalPolicy()
	}
	return m, nil
}

// authorLocalPolicy writes a working routing policy into the app-owned
// config.json for the models this configuration actually uses.
//
// This is the escape hatch that keeps the fail-closed refusal from being a
// brick on a host ansible does not manage. Every route it writes is on a wire
// this build can speak, so the policy it authors works rather than merely
// parsing. On a managed host the write still happens and is still inert,
// because the managed file wins — and the notice says so outright, since a
// user who edits a local policy and sees nothing change has no other way to
// tell why.
func (m *Model) authorLocalPolicy() {
	cfg := m.runtime.Config()
	models := []string{m.modelSeat, m.modelSubagent, m.modelLeaf, cfg.SeatModel, cfg.SubagentModel, cfg.LeafModel}
	models = append(models, cfg.ApprovedModels...)
	policy := provider.DefaultLocalPolicy(models)
	if len(policy.Routes) == 0 {
		m.modelNotice = "no models selected yet, so there is no route to author a policy for"
		m.modelNoticeErr = true
		m.modelNoticeOK = false
		return
	}
	reply, err := m.runtime.Do(seam.AuthorLocalPolicyCommand{Policy: policy})
	if err != nil {
		m.modelNotice = "author local policy: " + err.Error()
		m.modelNoticeErr = true
		m.modelNoticeOK = false
		return
	}
	routes := len(policy.Routes)
	if reply.PolicySource.Kind == config.PolicyManaged {
		m.modelNotice = fmt.Sprintf(
			"local policy written for %d route(s), but it is not in force: the managed policy at %s wins and slbh never writes that file",
			routes, reply.PolicySource.Path)
		m.modelNoticeErr = false
		m.modelNoticeOK = true
		return
	}
	m.modelNotice = fmt.Sprintf("local policy authored for %d route(s); policy source is now %s", routes, reply.PolicySource.Describe())
	m.modelNoticeErr = false
	m.modelNoticeOK = true
}

func (m *Model) updateModelSubmenu(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	options := []string{"seat", "subagent", "leaf"}
	switch msg.Code {
	case tea.KeyUp:
		if m.modelSubcursor > 0 {
			m.modelSubcursor--
		}
	case tea.KeyDown:
		if m.modelSubcursor < len(options)-1 {
			m.modelSubcursor++
		}
	case tea.KeyEnter:
		m.assignModel(m.modelSubnode, options[m.modelSubcursor])
		m.modelSubmenu = false
	case 'r', 'R':
		m.assignModel(m.modelSubnode, "seat")
		m.modelSubmenu = false
	case 's', 'S':
		m.assignModel(m.modelSubnode, "subagent")
		m.modelSubmenu = false
	case 'l', 'L':
		m.assignModel(m.modelSubnode, "leaf")
		m.modelSubmenu = false
	}
	return m, nil
}

func (m Model) modelNodes() []modelTreeNode {
	nodes := make([]modelTreeNode, 0)
	for i, catalog := range m.modelCatalog {
		nodes = append(nodes, modelTreeNode{provider: i, model: -1, branch: true})
		if !m.modelExpanded[catalog.Name] {
			continue
		}
		for j := range catalog.Models {
			nodes = append(nodes, modelTreeNode{provider: i, model: j})
		}
	}
	nodes = append(nodes, modelTreeNode{save: true})
	return nodes
}

func (m *Model) assignModel(node modelTreeNode, slot string) {
	model := m.modelCatalog[node.provider].Models[node.model].ID
	switch slot {
	case "seat":
		m.modelSeat = model
	case "subagent":
		m.modelSubagent = model
	case "leaf":
		m.modelLeaf = model
	default:
		return
	}
	if _, err := m.runtime.Do(seam.ConfigureModelSlotsCommand{SeatModel: m.modelSeat, SubagentModel: m.modelSubagent, LeafModel: m.modelLeaf, Approved: approvedSlots(m.modelSeat, m.modelSubagent, m.modelLeaf)}); err != nil {
		m.modelNotice = "save failed: " + err.Error()
		m.modelNoticeErr = true
		m.modelNoticeOK = false
		return
	}
	m.modelNotice = slot + " model set to " + displayModelID(m.modelCatalog[node.provider].Name, model)
	m.modelNoticeErr = false
	m.modelNoticeOK = true
}

func (m *Model) saveAndCloseModelMenu() {
	if _, err := m.runtime.Do(seam.ConfigureModelSlotsCommand{SeatModel: m.modelSeat, SubagentModel: m.modelSubagent, LeafModel: m.modelLeaf, Approved: approvedSlots(m.modelSeat, m.modelSubagent, m.modelLeaf)}); err != nil {
		m.modelNotice = "save failed: " + err.Error()
		m.modelNoticeErr = true
		m.modelNoticeOK = false
		return
	}
	m.modelsOpen = false
	m.modelsLoading = false
	m.modelSubmenu = false
	m.input.Focus()
	m.addLocal("status", "model configuration saved")
}

func approvedSlots(seat, subagent, leaf string) []string {
	result := make([]string, 0, 3)
	for _, model := range []string{seat, subagent, leaf} {
		if model != "" && !containsModel(result, model) {
			result = append(result, model)
		}
	}
	return result
}

func (m Model) modelSlotMarker(model string) string {
	markers := make([]string, 0, 3)
	if strings.EqualFold(m.modelSeat, model) && model != "" {
		markers = append(markers, "r")
	}
	if strings.EqualFold(m.modelSubagent, model) && model != "" {
		markers = append(markers, "s")
	}
	if strings.EqualFold(m.modelLeaf, model) && model != "" {
		markers = append(markers, "l")
	}
	return strings.Join(markers, "/")
}

func modelSlotValue(model string) string {
	if model == "" {
		return "none"
	}
	return model
}

func (m Model) modelSubmenuView() string {
	width := max(1, m.width)
	model := m.modelCatalog[m.modelSubnode.provider].Models[m.modelSubnode.model]
	options := []string{"seat", "subagent", "leaf"}
	lines := []string{
		accent.Render("ASSIGN MODEL"),
		"Choose a slot for " + displayModelID(m.modelCatalog[m.modelSubnode.provider].Name, model.ID) + ":",
		"",
	}
	for i, option := range options {
		prefix := "  "
		if i == m.modelSubcursor {
			prefix = "> "
		}
		lines = append(lines, prefix+option)
	}
	lines = append(lines, "", dim.Render("Enter assign · r/s/l assign directly · Esc back"))
	return wrapToWidth(strings.Join(lines, "\n"), width)
}

func displayModelID(providerName, model string) string {
	prefix := strings.ToLower(strings.TrimSpace(providerName)) + "/"
	if strings.HasPrefix(strings.ToLower(model), prefix) {
		return model[len(prefix):]
	}
	return model
}

func containsModel(models []string, wanted string) bool {
	for _, model := range models {
		if strings.EqualFold(strings.TrimSpace(model), strings.TrimSpace(wanted)) {
			return true
		}
	}
	return false
}

func (m Model) modelMenuView() string {
	if m.modelSubmenu {
		return m.modelSubmenuView()
	}
	width := max(1, m.width)
	lines := []string{
		accent.Render("MODELS"),
		"Providers with configured keys; OpenRouter shows models created within the last year.",
		fmt.Sprintf("Slots: seat=%s · subagent=%s · leaf=%s", modelSlotValue(m.modelSeat), modelSlotValue(m.modelSubagent), modelSlotValue(m.modelLeaf)),
		// Which policy is in force, and from where. A user on a managed host
		// who edits a local policy sees no change, and this line is the only
		// thing that tells them the managed file is winning.
		wrapToWidth("Routing policy: "+m.runtime.PolicySource().Describe(), width),
		// The per-layer role documents, from the same managed directory. A
		// converge that did not land leaves agents running on baked mechanics
		// alone, which is a legitimate state and therefore a silent one unless
		// it is reported here.
		wrapToWidth("Layer instructions: "+m.runtime.InstructionSource().Describe(), width),
		// Skill loading degrades independently of the layer documents. Report
		// absent directories and rejected skill metadata rather than silently
		// presenting a smaller role inventory.
		wrapToWidth("Layer skills: "+m.runtime.SkillSource().Describe(), width),
		"r=seat · s=subagent · l=leaf · p=author local policy · Enter=assign · Esc=save and close",
		"",
	}
	if m.modelsLoading {
		lines = append(lines, "Loading provider catalogs…")
	} else {
		if len(m.modelCatalog) == 0 {
			lines = append(lines, "No provider keys found or no models returned.")
		}
		nodes := m.modelNodes()
		available := max(1, m.height-len(lines)-2)
		start := max(0, m.modelCursor-available/2)
		if start+available > len(nodes) {
			start = max(0, len(nodes)-available)
		}
		end := min(len(nodes), start+available)
		for i := start; i < end; i++ {
			node := nodes[i]
			prefix := "  "
			if i == m.modelCursor {
				prefix = "> "
			}
			if node.save {
				lines = append(lines, accent.Render(wrapToWidth(prefix+"Save and Close", width)))
				continue
			}
			catalog := m.modelCatalog[node.provider]
			if node.branch {
				marker := "▸"
				if m.modelExpanded[catalog.Name] {
					marker = "▾"
				}
				line := fmt.Sprintf("%s%s %s (%d models)", prefix, marker, catalog.Name, len(catalog.Models))
				if catalog.Err != "" {
					line += " [" + catalog.Err + "]"
				}
				lines = append(lines, dim.Render(wrapToWidth(line, width)))
				continue
			}
			model := catalog.Models[node.model]
			lines = append(lines, wrapToWidth(fmt.Sprintf("%s  [%s] %s", prefix, m.modelSlotMarker(model.ID), displayModelID(catalog.Name, model.ID)), width))
		}
	}
	if m.modelNotice != "" {
		notice := dim
		if m.modelNoticeErr {
			notice = red
		} else if m.modelNoticeOK {
			notice = green
		}
		lines = append(lines, "", notice.Render(wrapToWidth(m.modelNotice, width)))
	}
	return strings.Join(lines, "\n")
}
