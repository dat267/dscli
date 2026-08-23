package cmd

// CLI is the root CLI struct containing all subcommand groups.
type CLI struct {
	ConfigFile string         `help:"Config file path" json:"-"`
	Chat       ChatCmd        `cmd:"" help:"Chat with DeepSeek (omit the prompt for an interactive session)"`
	Translate  TranslateCmd   `cmd:"" help:"Translate a file (txt, md, lrc, srt, vtt, ass, ttml, epub) via the model"`
	Login      LoginCmd       `cmd:"" help:"Show how to capture your DeepSeek login (token + cookie)"`
	Version    VersionCmd     `cmd:"" help:"Show version"`
	Config     ConfigCmdGroup `cmd:"" help:"Manage application configuration"`
}
