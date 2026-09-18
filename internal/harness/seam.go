package harness

import (
	"fmt"

	"github.com/slbdotdev/slbh/internal/config"
	"github.com/slbdotdev/slbh/internal/provider"
	"github.com/slbdotdev/slbh/internal/seam"
)

// Do dispatches the seam's closed command set. The command event is emitted
// before execution so failures remain part of the ordered runtime record.
func (r *Runtime) Do(command seam.Command) (seam.Reply, error) {
	if command == nil {
		return seam.Reply{}, fmt.Errorf("unknown command %q", "<nil>")
	}
	name := command.CommandName()
	r.emit(seam.Event{Kind: "command", Text: name, Metadata: map[string]any{"name": name}})
	if !seam.IsCommandName(name) {
		return seam.Reply{}, fmt.Errorf("unknown command %q", name)
	}
	reply := seam.Reply{Command: name}
	switch command := command.(type) {
	case seam.SendPromptCommand:
		return reply, r.sendPrompt(command.AgentID, command.Prompt)
	case seam.SteerAgentCommand:
		return reply, r.steerAgent(command.AgentID, command.Message)
	case seam.SetAgentEffortCommand:
		if err := r.setAgentEffort(command.AgentID, command.Effort); err != nil {
			return reply, err
		}
		if !command.Persist {
			return reply, nil
		}
		r.mu.Lock()
		if command.AgentID != r.seatID {
			r.mu.Unlock()
			return reply, fmt.Errorf("set_agent_effort: only the seat effort can be persisted")
		}
		r.config.SeatEffort = command.Effort
		cfg := cloneConfig(r.config)
		r.mu.Unlock()
		return reply, cfg.Save()
	case seam.ClearCommand:
		return reply, r.clear(command.AgentID)
	case seam.CompactCommand:
		dropped, err := r.compact(command.AgentID, command.Keep)
		reply.Dropped = dropped
		return reply, err
	case seam.ConfigureModelsCommand:
		return reply, r.configureModels(command.SeatModel, command.SubagentModel, command.Approved)
	case seam.ConfigureModelSlotsCommand:
		return reply, r.configureModelSlots(command.SeatModel, command.SubagentModel, command.LeafModel, command.Approved)
	case seam.SetModelCatalogCommand:
		r.setModelCatalog(command.Catalog)
		return reply, nil
	case seam.AuthorLocalPolicyCommand:
		source, err := r.authorLocalPolicy(command.Policy)
		reply.PolicySource = source
		return reply, err
	case seam.EmitStatusCommand:
		r.emitStatus(command.Kind, command.Text)
		return reply, nil
	case seam.CloseCommand:
		return reply, r.Close()
	default:
		return reply, fmt.Errorf("unsupported command %q", name)
	}
}

func cloneConfig(cfg config.Config) config.Config {
	cloned := cfg
	cloned.ApprovedModels = append([]string(nil), cfg.ApprovedModels...)
	cloned.Policy = clonePolicy(cfg.Policy)
	if cfg.LocalPolicy != nil {
		local := clonePolicy(*cfg.LocalPolicy)
		cloned.LocalPolicy = &local
	}
	if cfg.Instructions.Layers != nil {
		cloned.Instructions.Layers = make(map[string]string, len(cfg.Instructions.Layers))
		for layer, text := range cfg.Instructions.Layers {
			cloned.Instructions.Layers[layer] = text
		}
	}
	cloned.Instructions.Source.Missing = append([]string(nil), cfg.Instructions.Source.Missing...)
	if cfg.Skills.Layers != nil {
		cloned.Skills.Layers = make(map[string][]config.Skill, len(cfg.Skills.Layers))
		for layer, skills := range cfg.Skills.Layers {
			cloned.Skills.Layers[layer] = append([]config.Skill(nil), skills...)
		}
	}
	cloned.Skills.Source.Missing = append([]string(nil), cfg.Skills.Source.Missing...)
	if cfg.Roster.Names != nil {
		cloned.Roster.Names = make(map[string]config.Role, len(cfg.Roster.Names))
		for name, role := range cfg.Roster.Names {
			role.ModelsApproved = append([]string(nil), role.ModelsApproved...)
			cloned.Roster.Names[name] = role
		}
	}
	return cloned
}

func clonePolicy(policy provider.Policy) provider.Policy {
	cloned := policy
	if policy.Routes == nil {
		return cloned
	}
	cloned.Routes = make(map[string]provider.RoutePolicy, len(policy.Routes))
	for name, route := range policy.Routes {
		route.Effort.Levels = cloneStringMap(route.Effort.Levels)
		if route.Provider != nil {
			posture := *route.Provider
			posture.Ignore = append([]string(nil), route.Provider.Ignore...)
			if route.Provider.ZDR != nil {
				zdr := *route.Provider.ZDR
				posture.ZDR = &zdr
			}
			if route.Provider.MaxPrice != nil {
				price := *route.Provider.MaxPrice
				posture.MaxPrice = &price
			}
			route.Provider = &posture
		}
		cloned.Routes[name] = route
	}
	return cloned
}

func cloneStringMap(source map[string]string) map[string]string {
	if source == nil {
		return nil
	}
	cloned := make(map[string]string, len(source))
	for key, value := range source {
		cloned[key] = value
	}
	return cloned
}

func cloneCatalog(catalog []provider.Catalog) []provider.Catalog {
	if catalog == nil {
		return nil
	}
	cloned := make([]provider.Catalog, len(catalog))
	for i, branch := range catalog {
		cloned[i] = branch
		cloned[i].Models = append([]provider.ModelInfo(nil), branch.Models...)
	}
	return cloned
}
