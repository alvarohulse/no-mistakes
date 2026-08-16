import { appendFile, mkdir, rm, writeFile } from "node:fs/promises";
import path from "node:path";
import process from "node:process";

import { GitHub, Manifest, VERSION } from "release-please";

const EXPECTED_PATHS = [".release-please-manifest.json", "CHANGELOG.md"];
const EXPECTED_RELEASE_PLEASE_VERSION = "17.3.0";
const EXPECTED_HEAD_BRANCH = "release-please--branches--main";

const [repository, baseBranch, expectedBaseSHA, outputDirectory] = process.argv.slice(2);
const token = process.env.GH_TOKEN;
const githubOutput = process.env.GITHUB_OUTPUT;
if (!repository || !baseBranch || !expectedBaseSHA || !outputDirectory || !token || !githubOutput) {
  throw new Error(
    "usage: produce-release-pr.mjs <owner/repo> <base-branch> <base-sha> <output-dir> with GH_TOKEN and GITHUB_OUTPUT",
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
await requireExpectedBase("before release-please production");

const capturedChangeSets = [];
const buildChangeSet = github.buildChangeSet.bind(github);
github.buildChangeSet = async (...args) => {
  const changes = await buildChangeSet(...args);
  capturedChangeSets.push(changes);
  return changes;
};

const manifest = await Manifest.fromManifest(
  github,
  baseBranch,
  "release-please-config.json",
  ".release-please-manifest.json",
);
const pullRequests = await manifest.createPullRequests();
await requireExpectedBase("after release-please production");

if (pullRequests.length === 0) {
  await setOutput("prs_created", "false");
  process.stdout.write("Release Please did not create or update a pull request.\n");
  process.exit(0);
}
if (pullRequests.length !== 1 || capturedChangeSets.length !== 1) {
  throw new Error(
    `expected one published release pull request and change set, found ${pullRequests.length} and ${capturedChangeSets.length}`,
  );
}

const [pullRequest] = pullRequests;
if (pullRequest.baseBranchName !== baseBranch || pullRequest.headBranchName !== EXPECTED_HEAD_BRANCH) {
  throw new Error(
    `unexpected release pull request branches: ${pullRequest.baseBranchName} <- ${pullRequest.headBranchName}`,
  );
}

const [changes] = capturedChangeSets;
const changedPaths = [...changes.keys()].sort();
if (JSON.stringify(changedPaths) !== JSON.stringify(EXPECTED_PATHS)) {
  throw new Error(`unexpected release-please generated paths: ${changedPaths.join(", ")}`);
}

const resolvedOutputDirectory = path.resolve(outputDirectory);
if (resolvedOutputDirectory === path.parse(resolvedOutputDirectory).root) {
  throw new Error("release-please output directory cannot be a filesystem root");
}
await rm(resolvedOutputDirectory, { recursive: true, force: true });
await mkdir(resolvedOutputDirectory, { recursive: true });
for (const generatedPath of EXPECTED_PATHS) {
  const change = changes.get(generatedPath);
  if (!change || typeof change.content !== "string") {
    throw new Error(`release-please did not produce text content for ${generatedPath}`);
  }
  await writeFile(path.join(resolvedOutputDirectory, generatedPath), change.content, "utf8");
}

await setOutput("prs_created", "true");
await setOutput("pr", JSON.stringify(pullRequest));
process.stdout.write(
  `${JSON.stringify({ headBranch: pullRequest.headBranchName, paths: changedPaths })}\n`,
);

async function requireExpectedBase(phase) {
  const liveBaseSHA = await github.getBranchSha(baseBranch);
  if (liveBaseSHA !== expectedBaseSHA) {
    throw new Error(`${baseBranch} changed ${phase}; refusing to publish stale release output`);
  }
}

async function setOutput(name, value) {
  await appendFile(githubOutput, `${name}=${value}\n`, "utf8");
}
