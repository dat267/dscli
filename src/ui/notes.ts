/** Small stderr/stdout note helpers shared by commands (errors are soft notes, never thrown). */
import process from "node:process";

export function stderrNote(msg: string): void {
	process.stderr.write(msg);
}

export function stdoutNote(msg: string): void {
	process.stdout.write(msg);
}
