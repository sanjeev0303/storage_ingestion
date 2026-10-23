// The API ignores an action filter that has anything but a-z, "." and "_",
// so a bad value would quietly show unfiltered rows. Catch it before sending.
export function validateAction(value: string) {
  const action = value.trim();
  if (action === "") return null;
  if (/^[a-z._]+$/.test(action)) return null;
  return "Use lowercase letters, dots and underscores only, for example member.role_change or member. The API ignores anything else and would show every action.";
}
