import { ApiError, describeError, isApiError } from "@/lib/api/errors";

// The server's wording for these is terse; say what to do next.
export function describeMemberError(error: unknown) {
  if (isApiError(error)) {
    if (error.code === "last_owner") return "That would leave the organization without an active owner. Make someone else an owner first.";
    if (error.code === "escalation") return "You can only grant or manage roles whose permissions you already hold.";
  }
  return describeError(error);
}

// For ConfirmDialog, which shows describeError(error): keep the status and code, swap in the clearer message.
export function friendlyError(error: unknown) {
  if (!isApiError(error)) return error;
  return new ApiError({ status: error.status, code: error.code, message: describeMemberError(error) });
}
