import { appendFile } from "node:fs/promises";
import process from "node:process";

import { GitHub, Manifest, VERSION } from "release-please";

import { bindReleaseCreationToSHA } from "./exact-release.mjs";

const EXPECTED_RELEASE_PLEASE_VERSION = "17.3.0";

const [repository, baseBranch, expectedBaseSHA] = process.argv.slice(2);
const token = process.env.GH_TOKEN;
const githubOutput = process.env.GITHUB_OUTPUT;
if (!repository || !baseBranch || !expectedBaseSHA || !token || !githubOutput) {
  throw new Error(
    "usage: create-release.mjs <owner/repo> <base-branch> <base-sha> with GH_TOKEN and GITHUB_OUTPUT",
  );
}
if (!/^[0-9a-f]{40}$/.test(expectedBaseSHA)) {
  throw new Error(`invalid base commit: ${expectedBaseSHA}`);
}
if (VERSION !== EXPECTED_RELEASE_PLEASE_VERSION) {
  throw new Error(`release-please version ${VERSION} does not match ${EXPECTED_RELEASE_PLEASE_VERSION}`);
}

const [owner, repo, ...extra] = repository.split("/");
if (!owner || !repo || extra.length > 0) {
  throw new Error(`invalid repository identity: ${repository}`);
}

const github = await GitHub.create({ owner, repo, token, defaultBranch: baseBranch });
const liveBaseSHA = await github.getBranchSha(baseBranch);
if (liveBaseSHA !== expectedBaseSHA) {
  throw new Error(`${baseBranch} changed before release creation; refusing a stale run`);
}

// release-please normally discovers its target from the mutable base branch.
// Enforce the triggering commit again at the API mutation boundary so a push
// racing the discovery pass cannot create a tag or draft for a newer commit.
bindReleaseCreationToSHA(github, expectedBaseSHA);

const manifest = await Manifest.fromManifest(
  github,
  baseBranch,
  "release-please-config.json",
  ".release-please-manifest.json",
);
const releases = await manifest.createReleases();
if (!Array.isArray(releases) || releases.length > 1) {
  throw new Error(`expected at most one root release, found ${releases?.length ?? "invalid output"}`);
}

if (releases.length === 0) {
  await emitReleaseOutputs(false, "", "");
  process.stdout.write("Release Please did not create a release.\n");
  process.exit(0);
}

const [release] = releases;
if (
  release.path !== "." ||
  release.sha !== expectedBaseSHA ||
  typeof release.tagName !== "string" ||
  typeof release.version !== "string" ||
  release.tagName.includes("\n") ||
  release.version.includes("\n")
) {
  throw new Error("release-please returned an invalid root release");
}

await emitReleaseOutputs(true, release.tagName, release.version);
process.stdout.write(
  `${JSON.stringify({ sha: release.sha, tagName: release.tagName, version: release.version })}\n`,
);

async function emitReleaseOutputs(created, tagName, version) {
  await appendFile(
    githubOutput,
    [
      `releases_created=${created}`,
      `release_created=${created}`,
      `tag_name=${tagName}`,
      `version=${version}`,
      `paths_released=${created ? '["."]' : "[]"}`,
      "",
    ].join("\n"),
    "utf8",
  );
}
