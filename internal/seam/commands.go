package seam

import "github.com/slbdotdev/slbh/internal/provider"

const (
	CommandSendPrompt          = "send_prompt"
	CommandSteerAgent          = "steer_agent"
	CommandSetAgentEffort      = "set_agent_effort"
	CommandClear               = "clear"
	CommandCompact             = "compact"
	CommandConfigureModels     = "configure_models"
	CommandConfigureModelSlots = "configure_model_slots"
	CommandSetModelCatalog     = "set_model_catalog"
	CommandAuthorLocalPolicy   = "author_local_policy"
	CommandEmitStatus          = "emit_status"
	CommandClose               = "close"
)

// Command is the closed set of state-changing requests accepted by Runtime.
// The concrete command types below are the only accepted implementations.
type Command interface {
	CommandName() string
}

type SendPromptCommand struct {
	AgentID string `json:"agent_id"`
	Prompt  string `json:"prompt"`
}

func (SendPromptCommand) CommandName() string { return CommandSendPrompt }

type SteerAgentCommand struct {
	AgentID string `json:"agent_id"`
	Message string `json:"message"`
}

func (SteerAgentCommand) CommandName() string { return CommandSteerAgent }

type SetAgentEffortCommand struct {
	AgentID string `json:"agent_id"`
	Effort  string `json:"effort"`
	Persist bool   `json:"persist,omitempty"`
}

func (SetAgentEffortCommand) CommandName() string { return CommandSetAgentEffort }

type ClearCommand struct {
	AgentID string `json:"agent_id"`
}

func (ClearCommand) CommandName() string { return CommandClear }

type CompactCommand struct {
	AgentID string `json:"agent_id"`
	Keep    int    `json:"keep"`
}

func (CompactCommand) CommandName() string { return CommandCompact }

type ConfigureModelsCommand struct {
	SeatModel     string   `json:"seat_model"`
	SubagentModel string   `json:"subagent_model"`
	Approved      []string `json:"approved"`
}

func (ConfigureModelsCommand) CommandName() string { return CommandConfigureModels }

type ConfigureModelSlotsCommand struct {
	SeatModel     string   `json:"seat_model"`
	SubagentModel string   `json:"subagent_model"`
	LeafModel     string   `json:"leaf_model"`
	Approved      []string `json:"approved"`
}

func (ConfigureModelSlotsCommand) CommandName() string { return CommandConfigureModelSlots }

type SetModelCatalogCommand struct {
	Catalog []provider.Catalog `json:"catalog"`
}

func (SetModelCatalogCommand) CommandName() string { return CommandSetModelCatalog }

type AuthorLocalPolicyCommand struct {
	Policy provider.Policy `json:"policy"`
}

func (AuthorLocalPolicyCommand) CommandName() string { return CommandAuthorLocalPolicy }

type EmitStatusCommand struct {
	Kind string `json:"kind"`
	Text string `json:"text"`
}

func (EmitStatusCommand) CommandName() string { return CommandEmitStatus }

type CloseCommand struct{}

func (CloseCommand) CommandName() string { return CommandClose }

type commandSpec struct {
	name string
	new  func() Command
}

var commandSpecs = []commandSpec{
	{CommandSendPrompt, func() Command { return SendPromptCommand{} }},
	{CommandSteerAgent, func() Command { return SteerAgentCommand{} }},
	{CommandSetAgentEffort, func() Command { return SetAgentEffortCommand{} }},
	{CommandClear, func() Command { return ClearCommand{} }},
	{CommandCompact, func() Command { return CompactCommand{} }},
	{CommandConfigureModels, func() Command { return ConfigureModelsCommand{} }},
	{CommandConfigureModelSlots, func() Command { return ConfigureModelSlotsCommand{} }},
	{CommandSetModelCatalog, func() Command { return SetModelCatalogCommand{} }},
	{CommandAuthorLocalPolicy, func() Command { return AuthorLocalPolicyCommand{} }},
	{CommandEmitStatus, func() Command { return EmitStatusCommand{} }},
	{CommandClose, func() Command { return CloseCommand{} }},
}

// CommandNames returns every command name in stable order.
func CommandNames() []string {
	names := make([]string, len(commandSpecs))
	for i, spec := range commandSpecs {
		names[i] = spec.name
	}
	return names
}

// NewCommand returns the zero value of the concrete command named by name.
func NewCommand(name string) (Command, bool) {
	for _, spec := range commandSpecs {
		if spec.name == name {
			return spec.new(), true
		}
	}
	return nil, false
}

// IsCommandName reports whether name belongs to the closed command set.
func IsCommandName(name string) bool {
	_, ok := NewCommand(name)
	return ok
}
