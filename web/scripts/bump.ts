// Cuts a release. Run it as `bun run bump [minor|major]`.
//
// The version lives in git tags such as v0.5.0. This script finds the newest
// tag and raises its last number. It raises the minor or major number
// instead when asked. The first release is always v1.0.0.
//
// The release is one commit named after the version. That commit gives the
// top section of CHANGELOG.md the version and today's date as its heading.
// The script tags the commit and pushes it with the tag.
//
// The pushed tag starts the release workflow. That workflow publishes the
// container image and the GitHub release.
//
// With --dry-run it prints the version it would release and stops there.

import { spawnSync } from "node:child_process";
import { existsSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";

const PARTS = ["major", "minor", "micro"] as const;
type Part = (typeof PARTS)[number];

// The heading that changes are noted under until they are released.
const CURRENT = /^## current version\b/i;

function fail(message: string): never {
  console.error(`bump: ${message}`);
  process.exit(1);
}

function git(...args: string[]): string {
  const out = spawnSync("git", args, { encoding: "utf8" });
  if (out.status !== 0) {
    fail(`git ${args.join(" ")} failed\n${out.stderr.trim()}`);
  }
  return out.stdout.trim();
}

// Returns the changelog with `heading` as its top section heading. A
// "Current version" heading is renamed. Without one, nothing was noted for
// this release, and the heading goes in above the previous release's.
function withHeading(changelog: string, heading: string): string {
  const lines = changelog.split("\n");
  const top = lines.findIndex((line) => line.startsWith("## "));
  if (top === -1) return `${changelog.trimEnd()}\n\n${heading}\n`;
  if (CURRENT.test(lines[top])) lines[top] = heading;
  else lines.splice(top, 0, heading, "");
  return lines.join("\n");
}

const args = process.argv.slice(2);
const dryRun = args.includes("--dry-run");
const words = args.filter((a) => a !== "--dry-run");
if (words.length > 1 || (words[0] && !PARTS.includes(words[0] as Part))) {
  fail("usage: bun run bump [minor|major] [--dry-run]");
}
const part: Part = (words[0] as Part) ?? "micro";

// A release is always cut from the tip of main upstream. Local changes
// would be left out of the tag without anyone noticing.
git("fetch", "--quiet", "--tags", "origin", "main");
if (!dryRun) {
  if (git("rev-parse", "--abbrev-ref", "HEAD") !== "main") {
    fail("not on main");
  }
  if (git("status", "--porcelain", "--untracked-files=no")) {
    fail("there are uncommitted changes");
  }
  if (git("rev-parse", "HEAD") !== git("rev-parse", "origin/main")) {
    fail("HEAD is not the tip of origin/main; push or pull first");
  }
}

const latest = git("tag", "--list", "v*", "--sort=-version:refname")
  .split("\n")
  .find((tag) => /^v\d+\.\d+\.\d+$/.test(tag));
let next = "v1.0.0";
if (latest) {
  let [major, minor, micro] = latest.slice(1).split(".").map(Number);
  if (part === "major") [major, minor, micro] = [major + 1, 0, 0];
  else if (part === "minor") [minor, micro] = [minor + 1, 0];
  else micro += 1;
  next = `v${major}.${minor}.${micro}`;
}

console.log(`${latest ?? "no release yet"} -> ${next}`);
if (dryRun) process.exit(0);

const now = new Date();
const pad = (n: number) => String(n).padStart(2, "0");
const today = `${now.getFullYear()}-${pad(now.getMonth() + 1)}-${pad(now.getDate())}`;

const path = join(git("rev-parse", "--show-toplevel"), "CHANGELOG.md");
const changelog = existsSync(path)
  ? readFileSync(path, "utf8")
  : "# Changelog\n";
writeFileSync(path, withHeading(changelog, `## ${next} (${today})`));

git("add", path);
git("commit", "--quiet", "--message", next);
git("tag", "--annotate", next, "--message", next);
git("push", "--quiet", "--atomic", "origin", "main", next);
console.log(`Pushed ${next}. The release workflow takes it from here.`);
