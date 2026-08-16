const COMMIT_SHA = /^[0-9a-f]{40}$/;

export function bindReleaseCreationToSHA(github, expectedSHA) {
  if (!COMMIT_SHA.test(expectedSHA)) {
    throw new Error(`invalid release commit: ${expectedSHA}`);
  }
  if (!github || typeof github.createRelease !== "function") {
    throw new Error("release-please GitHub client cannot create releases");
  }

  const createRelease = github.createRelease.bind(github);
  github.createRelease = async (release, options) => {
    if (release?.sha !== expectedSHA) {
      throw new Error(
        `release candidate ${release?.sha ?? "<missing>"} does not match the triggering commit ${expectedSHA}`,
      );
    }
    return createRelease({ ...release, sha: expectedSHA }, options);
  };
}
