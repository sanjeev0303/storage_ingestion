# Single sign-on (OpenID Connect)

An organization can let its people sign in through its own identity provider, such as Okta, Entra ID, Google Workspace, Auth0 or Keycloak: anything that speaks OpenID Connect discovery.

## Set-up (`org.manage`)

`PUT /api/v1/organization/sso`:

```json
{"issuer": "https://login.example.com/", "client_id": "…", "client_secret": "…",
 "allowed_domains": ["example.com"], "default_role": "developer", "enabled": true}
```

- **Issuer check:** the issuer's discovery document is read when the settings are saved. It must name exactly this issuer. Its endpoints must pass the network guard (HTTPS, no private or metadata addresses).
- **Client secret:** sealed with the credential keyring. It is never returned; leave it empty on an update to keep the stored one.
- **`allowed_domains`:** only verified emails (`email_verified`) in these domains may sign in.
- **`default_role`:** given to people signing in for the first time. Their account (with no password) and membership are created then. Empty means only existing members may sign in. The owner role cannot be given this way.
- **Read and remove:** `GET` returns the settings without the secret, plus `redirect_uris`, the callback addresses to register at the provider. `DELETE` turns SSO off.

The server needs `AUTH_SSO_REDIRECT_URIS`: the console's callback, for example `https://console.example.com/api/session/sso/callback`. A sign-in may return only there. Without it, or without the credential keyring, SSO answers 501.

## Sign-in

1. The console posts `{organization: <slug>, redirect_uri}` to `POST /api/v1/auth/sso/start`. The answer is the provider's `authorization_url`.
2. The provider sends the browser back to the console's callback with `code` and `state`. The console's server posts them to `POST /api/v1/auth/sso/callback`. The answer is the same tokens as a password sign-in, plus `organization_id`.

The server keeps the protections:

- **The flow:** the authorization code flow with PKCE (S256), a nonce, and the scopes `openid email profile`.
- **State:** single use, ten minutes, stored only as a hash.
- **Code exchange:** the code is exchanged with the client secret over the guarded client.
- **ID token:** its signature is checked against the provider's published keys (RS256/384/512, PS256, ES256/384, EdDSA). So are its issuer, its audience (and `azp` when there are several), its expiry and its nonce.
- **Account link:** the provider's subject is linked to the account on its first sign-in. A later change of email at the provider does not move it to another account. A second subject cannot take an account already linked.
- **Suspensions:** a suspended membership or account cannot sign in.

Every refusal is audited as `auth.sso_login` with its reason. Successful sign-ins are `auth.login`, as with passwords. Starting and finishing are rate-limited per address (`RATE_LIMIT_SSO`, 30 a minute).

## SAML

An organization's connection is OpenID Connect or SAML 2.0 (`"protocol": "SAML"`).

- **Set-up:** for SAML, `PUT /organization/sso` takes `idp_metadata`, the identity provider's metadata XML (at most 256 KiB), instead of the issuer, client ID and secret. Leave it empty on an update to keep the stored metadata. The metadata must name the provider (`entityID`), offer an HTTP-Redirect single sign-on address, and carry a signing certificate. `GET /organization/sso` returns `saml.entity_id` (`urn:object-storage:sp:<org id>`) and `saml.acs_url` to register at the provider.
- **Server setting:** `AUTH_SAML_ACS_URL` is the console's ACS route, for example `https://console.example.com/api/session/saml/acs`. Without it, SAML is off.

**Sign-in:**

1. The same `POST /auth/sso/start` returns the provider's address. It carries an AuthnRequest by the HTTP-Redirect binding, and its RelayState is a single-use, ten-minute state (only its hash is kept).
2. The provider posts the response to the console's ACS route.
3. The console's server forwards it to `POST /auth/saml/acs {saml_response, relay_state}`.

**Checks:** [crewjam/saml](https://github.com/crewjam/saml) checks the XML signature against the certificate in the metadata. It also checks the audience (this organization's entity ID), the destination (the ACS URL), the times, and `InResponseTo` against the request this sign-in made. So a response is used once, for the sign-in it answers. Sign-ins the provider starts itself are refused. The reason a response failed is logged, never shown.

**Who signed in:** the NameID is the subject. The email comes from an email attribute (`email`, `mail`, the Microsoft claim or the `urn:oid` one), or else from an email-shaped NameID. The provider vouches for the address by signing it. The allowed domains, default role and account linking then work as for OpenID Connect.

## SCIM

SCIM 2.0 lets an identity provider keep the organization's members in step with its directory. Okta, Entra ID and OneLogin all speak it.

- **Token:** `POST /api/v1/organization/scim-token` (`org.manage`) makes the organization's token (`sst_…`). It is shown once and only its hash is kept; making a new one replaces it. `GET` says whether a token exists and when it was made and last used; `DELETE` revokes it.
- **Base URL:** `<console>/scim/v2`. The console passes the provider's bearer token through to the API.

| Resource | Supported |
|---|---|
| `GET /scim/v2/ServiceProviderConfig` | yes |
| `GET /scim/v2/Users` | `filter=userName eq "…"`, `startIndex`, `count` (up to 200) |
| `POST /scim/v2/Users` | the person joins: an existing account becomes a member; a new one is made without a password, to sign in through the organization's identity provider. The role is the SSO connection's default role, or developer. With SSO configured, the email must be in its allowed domains. `active: false` joins suspended. An existing member is 409 `uniqueness`. |
| `GET`, `PUT`, `PATCH /scim/v2/Users/{id}` | `active` suspends or reactivates the membership. Both PATCH shapes providers send are accepted (`path: "active"`, or `value: {"active": …}`), and so is `"True"`/`"False"` as a string. Name and email changes are ignored: they belong to the account, not the directory. |
| `DELETE /scim/v2/Users/{id}` | removes the membership; the account stays |
| `GET /scim/v2/Groups`, `/Groups/{id}` | every role, built-in and custom, with its members |
| `PATCH /scim/v2/Groups/{id}` | `add` members gives them that role. `remove` gives them back the default role (or developer). |

**Never on owners:** SCIM never touches owners. It cannot change, suspend or remove an owner, nor give the owner role (403 `mutability`).

**Other limits:** another organization's people are not found (404). Calls are rate-limited per address and audited, with `scim` as the actor.
