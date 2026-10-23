import { describe, expect, it } from "vitest";
import { ApiError, describeError, explainError, readError } from "./errors";

const failure = (status: number, message = "x", extra: Partial<ConstructorParameters<typeof ApiError>[0]> = {}) =>
  new ApiError({ status, message, ...extra });

describe("explainError", () => {
  it.each([
    [0, "Could not reach the server."],
    [401, "Your session has ended."],
    [403, "Your role cannot do this."],
    [404, "That could not be found."],
    [429, "Too many requests."],
    [500, "Something went wrong on our side."],
    [503, "Something went wrong on our side."],
  ])("tells a %s as what happened, why, and what to do", (status, what) => {
    const explained = explainError(failure(status));
    expect(explained.what).toBe(what);
    expect(explained.why).not.toBe("");
    expect(explained.action).not.toBe("");
  });

  it("never repeats the server's own words for an internal failure", () => {
    const explained = explainError(failure(500, "pq: duplicate key violates constraint users_pkey", { requestId: "req-1" }));
    expect(JSON.stringify(explained)).not.toContain("pq:");
    expect(explained.reference).toBe("req-1");
  });

  it("keeps the server's wording for a rejected field, which says what to fix", () => {
    expect(describeError(failure(400, "weak password: at least 12 characters"))).toBe("weak password: at least 12 characters");
  });

  it("explains an error that is not an API error without exposing it", () => {
    const text = describeError(new TypeError("Cannot read properties of undefined (reading 'id')"));
    expect(text).not.toContain("undefined");
    expect(text).toMatch(/Try again/);
  });

  it("tells how long to wait when the server says so", () => {
    expect(explainError(failure(429, "x", { retryAfter: 12 })).action).toBe("Wait 12 seconds and try again.");
  });
});

describe("readError", () => {
  it("drops the body of a 5xx so the cause stays on the server", async () => {
    const res = new Response(JSON.stringify({ error: "pq: connection refused", code: "internal" }), { status: 500 });
    const error = await readError(res);
    expect(error.message).not.toContain("pq:");
    expect(error.body).toBeUndefined();
  });

  it("keeps the message of a 4xx", async () => {
    const res = new Response(JSON.stringify({ error: "name is taken", code: "conflict" }), { status: 409 });
    expect((await readError(res)).message).toBe("name is taken");
  });
});
