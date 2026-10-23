// Runs in the browser before the app. In production a caught or uncaught Error
// must not be printed to the console: details belong in the server log, which
// the error pages and ErrorReporter feed through /api/client-error.
if (process.env.NODE_ENV === "production") {
  const original = console.error;
  console.error = (...args: unknown[]) => {
    if (args.some((arg) => arg instanceof Error)) return;
    original(...args);
  };
}
