package ui

// ShellView is the complete, presentation-only state for the Waffle Desk shell.
type ShellView struct {
	Title         string
	ActiveSection string
	Connection    string
	ModelAlias    string
	RequestToken  string
	AssetVersion  string
}

// FragmentView is the small presentation model shared by the four htmx
// sections. Values are already reduced to the public Desk projection; templ
// escapes every value before it reaches the response body.
type FragmentView struct {
	ID          string
	Class       string
	Empty       string
	Status      string
	GetURL      string
	Trigger     string
	Items       []FragmentItem
	OptionLists []FragmentOptionList
	TextSwaps   []FragmentTextSwap
	Filters     []FragmentFilter
}

type FragmentItem struct {
	ID                     string
	Class                  string
	Kind                   string
	Title                  string
	Detail                 string
	DetailClass            string
	DataTaskID             string
	DataWorkspaceID        string
	DataTaskName           string
	DataTaskCron           string
	DataTaskPrompt         string
	DataTaskDeliver        string
	DataTaskProfile        string
	DataTaskEnabled        bool
	DataTaskRedactedFields string
	Fields                 []FragmentField
	Actions                []FragmentAction
}

type FragmentField struct {
	Label string
	Value string
}

type FragmentOptionList struct {
	ID       string
	Name     string
	Required bool
	Options  []FragmentOption
}

type FragmentOption struct {
	Value string
	Label string
}

type FragmentTextSwap struct {
	ID        string
	Text      string
	Class     string
	Source    string
	Available bool
}

type FragmentFilter struct {
	ID      string
	Name    string
	URL     string
	Label   string
	Pressed bool
}

type FragmentAction struct {
	ID      string
	Label   string
	Method  string
	URL     string
	Target  string
	Swap    string
	Class   string
	Include string
	Fields  []FragmentField
	Inputs  []FragmentInput
}

type FragmentInput struct {
	ID          string
	Name        string
	Type        string
	Label       string
	Placeholder string
	Value       string
	Required    bool
}

type FragmentStatusView struct {
	Message         string
	Error           bool
	RestartRequired bool
}

type WorkspaceCloseDialogView struct {
	Repository   string
	Dirty        string
	Unpushed     string
	PreviewURL   string
	PreviewToken string
	Eligible     bool
	FocusID      string
}

type MemoryForgetDialogView struct {
	NoteID       string
	Excerpt      string
	Scope        string
	Excludes     []string
	PreviewURL   string
	PreviewToken string
}

type SkillReviewFileView struct {
	Path    string
	Size    string
	SHA256  string
	Preview string
}

type SkillReviewView struct {
	Name          string
	Description   string
	SourceRef     string
	ContentDigest string
	Files         []SkillReviewFileView
	AuditPassed   bool
	AuditTitle    string
	AuditMessage  string
	Flags         []string
	StageID       string
	ExpiresAt     string
}

func fragmentBool(value bool) string {
	if value {
		return "true"
	}
	return "false"
}
