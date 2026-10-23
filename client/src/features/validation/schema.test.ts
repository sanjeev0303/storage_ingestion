import { describe, expect, it } from "vitest";
import { buildProfileRequest, buildStartRequest, parseConfig, type ProfileDraft, type StartForm } from "./schema";

const base: StartForm = { mode: "METADATA", sampleSize: "", versionId: "v1", profileId: "builtin:general", profileVersion: 1 };

describe("buildStartRequest", () => {
  it("never sends sample_size unless the mode is SAMPLED", () => {
    for (const mode of ["METADATA", "FULL"] as const) {
      const { request } = buildStartRequest({ ...base, mode, sampleSize: "50" }, "key", { profileRequired: false });
      expect(request).toBeDefined();
      expect(request).not.toHaveProperty("sample_size");
    }
  });

  it("sends a sample size with SAMPLED and accepts 1 to 10000", () => {
    for (const size of ["1", "100", "10000"]) {
      const { request } = buildStartRequest({ ...base, mode: "SAMPLED", sampleSize: size }, "key", { profileRequired: false });
      expect(request?.sample_size).toBe(Number(size));
    }
  });

  it("leaves sample_size out when empty so the server default applies", () => {
    const { request } = buildStartRequest({ ...base, mode: "SAMPLED" }, "key", { profileRequired: false });
    expect(request).not.toHaveProperty("sample_size");
  });

  it.each(["0", "-3", "10001", "2.5", "abc"])("refuses sample size %s", (size) => {
    const { request, errors } = buildStartRequest({ ...base, mode: "SAMPLED", sampleSize: size }, "key", { profileRequired: false });
    expect(request).toBeUndefined();
    expect(errors.sampleSize).toBeTruthy();
  });

  it("requires a profile for a readiness assessment only", () => {
    const form = { ...base, profileId: "", profileVersion: undefined };
    expect(buildStartRequest(form, "k", { profileRequired: true }).request).toBeUndefined();
    expect(buildStartRequest(form, "k", { profileRequired: false }).request).toBeDefined();
  });

  it("carries the idempotency key and the chosen version and profile", () => {
    const { request } = buildStartRequest(base, "abc", { profileRequired: false });
    expect(request).toEqual({
      mode: "METADATA",
      idempotency_key: "abc",
      version_id: "v1",
      profile_id: "builtin:general",
      profile_version: 1,
    });
  });
});

const draft: ProfileDraft = {
  name: " Images ",
  description: "",
  maxErrors: "0",
  maxWarnings: "",
  rules: [{ id: "meta", version: "1", type: "required_metadata", severity: "ERROR", enabled: true, scope: "", config: '{"keys":["license"]}' }],
};

describe("buildProfileRequest", () => {
  const types = ["required_metadata", "member_count"];

  it("builds the strict body: trimmed name, parsed config, no empty optional fields", () => {
    const { request } = buildProfileRequest(draft, types);
    expect(request).toEqual({
      name: "Images",
      rules: [{ id: "meta", version: 1, type: "required_metadata", severity: "ERROR", enabled: true, config: { keys: ["license"] } }],
      thresholds: { max_errors: 0 },
    });
  });

  it("requires config to be a JSON object", () => {
    expect(parseConfig("").value).toBeUndefined();
    expect(parseConfig("[1]").error).toBeTruthy();
    expect(parseConfig("not json").error).toBeTruthy();
    expect(parseConfig('{"a":1}').value).toEqual({ a: 1 });
  });

  it("reports rule problems against the rule index", () => {
    const bad = { ...draft, rules: [{ ...draft.rules[0], type: "nonsense", config: "[]" }, { ...draft.rules[0], id: "meta" }] };
    const { request, errors } = buildProfileRequest(bad, types);
    expect(request).toBeUndefined();
    expect(errors.rule[0]).toMatchObject({ type: expect.any(String), config: expect.any(String) });
    expect(errors.rule[1].id).toMatch(/unique/);
  });

  it("checks thresholds", () => {
    expect(buildProfileRequest({ ...draft, maxErrors: "-1" }, types).errors.maxErrors).toBeTruthy();
    expect(buildProfileRequest({ ...draft, maxWarnings: "x" }, types).errors.maxWarnings).toBeTruthy();
    expect(buildProfileRequest({ ...draft, maxWarnings: "5" }, types).request?.thresholds).toEqual({ max_errors: 0, max_warnings: 5 });
  });
});
