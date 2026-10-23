// The server's real password rules (identity checkPassword): a minimum length,
// at most 256 bytes, and not the email address. Nothing else is required, so
// nothing else is promised here.
export const PASSWORD_MIN_LENGTH = 12;

export type PasswordRule = { label: string; met: boolean };

export function passwordRules(
  password: string,
  options: { email?: string; current?: string; repeat?: string } = {},
): PasswordRule[] {
  const rules: PasswordRule[] = [
    { label: `At least ${PASSWORD_MIN_LENGTH} characters`, met: [...password].length >= PASSWORD_MIN_LENGTH },
  ];
  if (options.email !== undefined) {
    rules.push({
      label: "Not your email address",
      met: password !== "" && password.toLowerCase() !== options.email.trim().toLowerCase(),
    });
  }
  if (options.current !== undefined) {
    rules.push({ label: "Different from the current password", met: password !== "" && password !== options.current });
  }
  if (options.repeat !== undefined) {
    rules.push({ label: "Matches the repeated password", met: password !== "" && password === options.repeat });
  }
  return rules;
}

export function passwordOk(rules: PasswordRule[]) {
  return rules.every((rule) => rule.met);
}
