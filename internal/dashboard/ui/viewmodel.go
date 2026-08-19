package ui

import "github.com/a-h/templ"

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
	EmptyState  *EmptyStateView
	Status      string
	GetURL      string
	Trigger     string
	SummaryID   string
	SummaryText string
	Items       []FragmentItem
	OptionLists []FragmentOptionList
	TextSwaps   []FragmentTextSwap
	Footers     []FragmentTextSwap
	Filters     []FragmentFilter
	// TaskScheduleOpenPrimary lets the Tasks fragment update the one stable
	// schedule trigger without duplicating it inside an empty state.
	TaskScheduleOpenPrimary bool
}

func fragmentLiveAttributes(live bool) templ.Attributes {
	if !live {
		return nil
	}
	return templ.Attributes{"aria-live": "polite"}
}

// EmptyStateKey names the small set of approved Waffle Desk illustrations.
// Consumers choose a semantic state rather than selecting artwork directly.
type EmptyStateKey string

const (
	EmptyStateTasks      EmptyStateKey = "tasks"
	EmptyStateWorkspaces EmptyStateKey = "workspaces"
	EmptyStateMemory     EmptyStateKey = "memory"
)

type emptyStateArtwork struct {
	AssetName string
	Width     string
	Height    string
	Class     string
}

type EmptyStateView struct {
	Title           string
	Body            string
	TitleID         string
	PrimaryAction   *FragmentAction
	SecondaryAction *FragmentAction
	NoArtwork       bool
	artwork         emptyStateArtwork
}

func emptyStateArtworkFor(key EmptyStateKey) (emptyStateArtwork, bool) {
	switch key {
	case EmptyStateTasks:
		return emptyStateArtwork{
			AssetName: "waffle-empty-curled.png",
			Width:     "480",
			Height:    "320",
			Class:     "waffle-empty-state-art-tasks",
		}, true
	case EmptyStateWorkspaces:
		return emptyStateArtwork{
			AssetName: "waffle-empty-sitting.png",
			Width:     "320",
			Height:    "320",
			Class:     "waffle-empty-state-art-workspaces",
		}, true
	case EmptyStateMemory:
		return emptyStateArtwork{
			AssetName: "waffle-empty-curious.png",
			Width:     "256",
			Height:    "256",
			Class:     "waffle-empty-state-art-memory",
		}, true
	default:
		return emptyStateArtwork{}, false
	}
}

// NewWaffleEmptyStateView combines approved artwork with copy owned by the
// section consumer. Unknown semantic keys are rejected rather than falling
// back to an unapproved illustration.
func NewWaffleEmptyStateView(key EmptyStateKey, title, body, titleID string, primaryAction, secondaryAction *FragmentAction) (EmptyStateView, bool) {
	artwork, ok := emptyStateArtworkFor(key)
	if !ok {
		return EmptyStateView{}, false
	}
	return EmptyStateView{Title: title, Body: body, TitleID: titleID, PrimaryAction: primaryAction, SecondaryAction: secondaryAction, artwork: artwork}, true
}

type FragmentItem struct {
	ID          string
	Class       string
	Kind        string
	Title       string
	Detail      string
	DetailClass string
	// Excerpt is an optional bounded readable body rendered below the item's
	// metadata; ExcerptLong adds a native details toggle that expands the
	// clamped text instead of hiding it (#458).
	Excerpt                string
	ExcerptLong            bool
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
	Live      bool
}

// MemorySessionOption is one eligible persisted session choice for the
// memory attach picker (#459). The label is human-readable; the opaque
// identifier is only the form value.
type MemorySessionOption struct {
	ID    string
	Label string
}

type MemorySessionPickerView struct {
	Choices []MemorySessionOption
}

type FragmentFilter struct {
	ID      string
	Name    string
	URL     string
	Label   string
	Pressed bool
}

type FragmentAction struct {
	ID         string
	Label      string
	Method     string
	URL        string
	Target     string
	Swap       string
	Class      string
	Include    string
	Disabled   bool
	Pressed    bool
	HasPressed bool
	// Value carries the copy target for Method "copy" actions (#462).
	Value  string
	Fields []FragmentField
	Inputs []FragmentInput
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
	FocusID      string
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

func fragmentActionFormClass(action FragmentAction) string {
	if len(action.Inputs) == 0 {
		return ""
	}
	return "waffle-action-form-with-inputs"
}

func emptyStateArtworkClass(view EmptyStateView) string {
	if view.NoArtwork {
		return "waffle-empty-state-art is-hidden"
	}
	return "waffle-empty-state-art " + view.artwork.Class
}

func emptyStateClass(view EmptyStateView) string {
	if view.NoArtwork {
		return "waffle-empty-state is-no-artwork"
	}
	return "waffle-empty-state"
}
