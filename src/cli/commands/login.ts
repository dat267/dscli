/**
 * `dscli login`: prints instructions for capturing a DeepSeek session — the
 * bearer token from localStorage.userToken and the (HttpOnly) ds_session_id
 * cookie. Port of cmd/login.go.
 */
import process from "node:process";
import { VERSION } from "../../config.js";

/** loginSnippet: a console one-liner for the signed-in site (reads the token from localStorage). */
export function loginSnippet(): string {
	return `(() => { const t = JSON.parse(localStorage.getItem('userToken') || '{}').value;
const c = Object.fromEntries(document.cookie.split('; ').filter(Boolean).map(p => { const i = p.indexOf('='); return [p.slice(0, i), p.slice(i + 1)]; }));
console.log(JSON.stringify({ token: t || null, cookie: c.ds_session_id ?? null, visible_cookies: Object.keys(c) })); })();`;
}

export function loginCommand(): void {
	const w = (s = "") => process.stdout.write(s + "\n");
	w(`dscli uses your signed-in chat.deepseek.com session (free account, no API key).`);
	w();
	w(`1. Open https://chat.deepseek.com in a browser and sign in.`);
	w(`2. Get the TOKEN. Open the developer console (F12 -> Console) and paste:`);
	w();
	w(loginSnippet());
	w();
	w(`   It prints a JSON object; copy the "token" value.`);
	w();
	w(`3. Get the COOKIE. ds_session_id is an HttpOnly cookie, so the console`);
	w(`   cannot read it — copy it from DevTools instead:`);
	w(`   - F12 -> Network -> click any request to chat.deepseek.com -> Headers,`);
	w(`     then in Request Headers copy the value after "ds_session_id=" in the`);
	w(`     cookie line. Pasting the whole cookie line also works.`);
	w(`   - (or F12 -> Application -> Cookies -> https://chat.deepseek.com ->`);
	w(`     the ds_session_id row, and copy its Value.)`);
	w();
	w(`4. Save them:`);
	w();
	w(`   dscli config set token "<token>"`);
	w(`   dscli config set cookie "<cookie>"   # value, or a full "k=v; ..." header`);
	w();
	w(`The values live only in your config file (~/.config/dscli/dscli.json)`);
	w(`and can be rotated any time; alternatively set DS_TOKEN / DS_COOKIE env vars.`);
}

/** `dscli version`. */
export function versionCommand(): void {
	process.stdout.write(VERSION + "\n");
}

