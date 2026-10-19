// louisiane connector for pi and omp, written by `lal install`. It keeps this
// session on the louisiane agent mesh: `lal connector` holds the session's
// stream from the relay, and each message it receives is put into the
// conversation, as a follow-up when the agent is mid-turn.
import { spawn, type ChildProcess } from "node:child_process";
import { createInterface } from "node:readline";

const LAL = "__LAL__";
const KIND = "__KIND__";

export default function (pi: any) {
	let child: ChildProcess | undefined;
	let ctxRef: any;
	const pending: string[] = [];

	const flush = () => {
		if (pending.length === 0) return;
		const text = pending.splice(0).join("\n\n");
		try {
			if (!ctxRef || ctxRef.isIdle()) pi.sendUserMessage(text);
			else pi.sendUserMessage(text, { deliverAs: "followUp" });
		} catch {
			pending.unshift(text);
		}
	};

	pi.on("session_start", async (_event: any, ctx: any) => {
		ctxRef = ctx;
		if (child) return;
		child = spawn(LAL, ["connector", "--kind", KIND, "--pid", String(process.pid), "--jsonl"], {
			stdio: ["pipe", "pipe", "ignore"],
		});
		const lines = createInterface({ input: child.stdout! });
		lines.on("line", (line: string) => {
			let m: any;
			try {
				m = JSON.parse(line);
			} catch {
				return;
			}
			if (m.type === "connected") {
				process.env.LAL_MEMBER = m.member;
			} else if (m.type === "deliver") {
				pending.push(m.text);
				flush();
			}
		});
		child.on("exit", () => {
			child = undefined;
		});
	});

	pi.on("agent_end", async (_event: any, ctx: any) => {
		if (ctx) ctxRef = ctx;
		flush();
	});

	pi.on("session_shutdown", async () => {
		child?.kill();
		child = undefined;
	});
}
