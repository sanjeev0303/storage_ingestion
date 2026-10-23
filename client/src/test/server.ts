import { setupServer } from "msw/node";

// Component tests mock the API here. A passing test only shows the component
// handles the response shape it was given; the real backend is covered by the
// Playwright journey in e2e/.
export const server = setupServer();

export const API = "http://localhost:3100/api/proxy";
