package cmd

import (
	"fmt"
)

// LoginCmd prints instructions for capturing a DeepSeek session: the bearer
// token from localStorage.userToken and the ds_session_id cookie.
//
// The token is readable from JavaScript (localStorage), but ds_session_id is
// an HttpOnly cookie, so it must be copied from DevTools (Network or
// Application tab) rather than the console.
type LoginCmd struct{}

func (c *LoginCmd) Run(app *App) error {
	fmt.Println(`dscli uses your signed-in chat.deepseek.com session (free account, no API key).`)
	fmt.Println()
	fmt.Println(`1. Open https://chat.deepseek.com in a browser and sign in.`)
	fmt.Println(`2. Get the TOKEN. Open the developer console (F12 -> Console) and paste:`)
	fmt.Println()
	fmt.Println(loginSnippet())
	fmt.Println()
	fmt.Println(`   It prints a JSON object; copy the "token" value.`)
	fmt.Println()
	fmt.Println(`3. Get the COOKIE. ds_session_id is an HttpOnly cookie, so the console`)
	fmt.Println(`   cannot read it — copy it from DevTools instead:`)
	fmt.Println(`   - F12 -> Network -> click any request to chat.deepseek.com -> Headers,`)
	fmt.Println(`     then in Request Headers copy the value after "ds_session_id=" in the`)
	fmt.Println(`     cookie line. Pasting the whole cookie line also works.`)
	fmt.Println(`   - (or F12 -> Application -> Cookies -> https://chat.deepseek.com ->`)
	fmt.Println(`     the ds_session_id row, and copy its Value.)`)
	fmt.Println()
	fmt.Println(`4. Save them:`)
	fmt.Println()
	fmt.Println(`   dscli config set token "<token>"`)
	fmt.Println(`   dscli config set cookie "<cookie>"   # value, or a full "k=v; ..." header`)
	fmt.Println()
	fmt.Println(`The values live only in your config file (~/.config/dscli/dscli.json)`)
	fmt.Println(`and can be rotated any time; alternatively set DS_TOKEN / DS_COOKIE env vars.`)
	return nil
}

// loginSnippet is a console one-liner for the signed-in site. It reads the
// bearer token the web app keeps in localStorage.userToken and tries the
// ds_session_id cookie (which is normally HttpOnly and thus absent — that is
// expected; the cookie is captured from DevTools per the login instructions).
func loginSnippet() string {
	return `(() => { const t = JSON.parse(localStorage.getItem('userToken') || '{}').value;
const c = Object.fromEntries(document.cookie.split('; ').filter(Boolean).map(p => { const i = p.indexOf('='); return [p.slice(0, i), p.slice(i + 1)]; }));
console.log(JSON.stringify({ token: t || null, cookie: c.ds_session_id ?? null, visible_cookies: Object.keys(c) })); })();`
}