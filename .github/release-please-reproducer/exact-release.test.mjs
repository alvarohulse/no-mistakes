import assert from "node:assert/strict";
import test from "node:test";

import { bindReleaseCreationToSHA } from "./exact-release.mjs";

const TRIGGER_SHA = "a".repeat(40);
const NEWER_SHA = "b".repeat(40);

test("creates a release only for the triggering commit", async () => {
  const created = [];
  const github = {
    async createRelease(release) {
      created.push(release.sha);
      return { sha: release.sha };
    },
  };

  bindReleaseCreationToSHA(github, TRIGGER_SHA);
  const release = await github.createRelease({ sha: TRIGGER_SHA });

  assert.deepEqual(created, [TRIGGER_SHA]);
  assert.equal(release.sha, TRIGGER_SHA);
});

test("rejects a live-main race before release mutation", async () => {
  const created = [];
  const github = {
    async createRelease(release) {
      created.push(release.sha);
      return { sha: release.sha };
    },
  };

  bindReleaseCreationToSHA(github, TRIGGER_SHA);

  await assert.rejects(
    github.createRelease({ sha: NEWER_SHA }),
    /does not match the triggering commit/,
  );
  assert.deepEqual(created, []);
});
