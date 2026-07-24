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
