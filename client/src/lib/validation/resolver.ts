import type { FieldErrors, FieldValues, Resolver } from "react-hook-form";
import type { ZodType } from "zod";

// @hookform/resolvers is not used: its peer dependencies do not resolve with
// zod 4 here, and this is all it does for us.
export function zodResolver<T extends FieldValues>(schema: ZodType): Resolver<T> {
  return async (values) => {
    const result = schema.safeParse(values);
    if (result.success) return { values: result.data as T, errors: {} };

    const errors: Record<string, unknown> = {};
    for (const issue of result.error.issues) {
      const path = issue.path.map(String);
      let cursor = errors;
      path.forEach((key, index) => {
        if (index === path.length - 1) {
          if (!cursor[key]) cursor[key] = { type: issue.code, message: issue.message };
          return;
        }
        cursor[key] = (cursor[key] as Record<string, unknown>) ?? {};
        cursor = cursor[key] as Record<string, unknown>;
      });
    }
    return { values: {}, errors: errors as FieldErrors<T> };
  };
}
