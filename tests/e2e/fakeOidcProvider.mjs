// Minimal local OIDC provider for the optional E2E SSO spec (P5 batch D).
//
// It is started by playwright.config.ts only when AEGIS_E2E_OIDC=1, on
// 127.0.0.1:9099. It generates a throwaway RSA key, serves discovery/JWKS,
// auto-approves the authorization request (302 back to the callback with a
// one-time code) and issues an RS256 id_token at the token endpoint. The PKCE
// S256 verifier is checked server-side so the E2E run would fail if the
// backend sent a wrong verifier or challenge.
//
// Never exposed outside the disposable E2E environment; no state persists.
import { createServer } from 'node:http'
import { createHash, generateKeyPairSync, randomBytes, sign } from 'node:crypto'

const port = Number(process.env.AEGIS_FAKE_OIDC_PORT || 9099)
const base = `http://127.0.0.1:${port}`
const clientId = 'aegisflow-e2e'
const kid = 'e2e-kid-1'
const { privateKey, publicKey } = generateKeyPairSync('rsa', { modulusLength: 2048 })
const publishedJWK = publicKey.export({ format: 'jwk' })
// code -> { nonce, challenge, redirectURI } ; one-time use.
const codes = new Map()
let issued = 0

const b64url = (value) => Buffer.from(value).toString('base64url')

function signIDToken(claims) {
  const header = b64url(JSON.stringify({ alg: 'RS256', typ: 'JWT', kid }))
  const payload = b64url(JSON.stringify(claims))
  const signature = sign('sha256', Buffer.from(`${header}.${payload}`), privateKey).toString('base64url')
  return `${header}.${payload}.${signature}`
}

function sendJSON(response, status, body) {
  response.writeHead(status, { 'Content-Type': 'application/json', 'Cache-Control': 'no-store' })
  response.end(JSON.stringify(body))
}

function readBody(request) {
  return new Promise((resolve, reject) => {
    let body = ''
    request.setEncoding('utf8')
    request.on('data', (chunk) => {
      body += chunk
      if (body.length > 64 * 1024) reject(new Error('body too large'))
    })
    request.on('error', reject)
    request.on('end', () => resolve(body))
  })
}

const server = createServer(async (request, response) => {
  const url = new URL(request.url ?? '/', base)
  try {
    if (url.pathname === '/.well-known/openid-configuration') {
      return sendJSON(response, 200, {
        issuer: base,
        authorization_endpoint: `${base}/authorize`,
        token_endpoint: `${base}/token`,
        jwks_uri: `${base}/jwks`,
        response_types_supported: ['code'],
        id_token_signing_alg_values_supported: ['RS256'],
      })
    }
    if (url.pathname === '/jwks') {
      return sendJSON(response, 200, {
        keys: [{ kty: 'RSA', kid, use: 'sig', alg: 'RS256', n: publishedJWK.n, e: publishedJWK.e }],
      })
    }
    if (url.pathname === '/authorize') {
      const state = url.searchParams.get('state') ?? ''
      const nonce = url.searchParams.get('nonce') ?? ''
      const challenge = url.searchParams.get('code_challenge') ?? ''
      const redirectURI = url.searchParams.get('redirect_uri') ?? ''
      if (!state || !nonce || !challenge || !redirectURI || url.searchParams.get('code_challenge_method') !== 'S256') {
        return sendJSON(response, 400, { error: 'invalid_request' })
      }
      const code = randomBytes(16).toString('base64url')
      codes.set(code, { nonce, challenge, redirectURI })
      const redirect = new URL(redirectURI)
      redirect.searchParams.set('code', code)
      redirect.searchParams.set('state', state)
      response.writeHead(302, { Location: redirect.toString(), 'Cache-Control': 'no-store' })
      return response.end()
    }
    if (url.pathname === '/token') {
      if (request.method !== 'POST') return sendJSON(response, 405, { error: 'invalid_method' })
      const form = new URLSearchParams(await readBody(request))
      const code = form.get('code') ?? ''
      const verifier = form.get('code_verifier') ?? ''
      const entry = codes.get(code)
      codes.delete(code)
      const expectedChallenge = createHash('sha256').update(verifier).digest('base64url')
      if (!entry || entry.challenge !== expectedChallenge || form.get('redirect_uri') !== entry.redirectURI || form.get('client_id') !== clientId) {
        return sendJSON(response, 400, { error: 'invalid_grant' })
      }
      issued += 1
      const now = Math.floor(Date.now() / 1000)
      const identity = {
        sub: `e2e-external-${now}-${issued}`,
        email: `e2e-sso-${now}-${issued}@example.test`,
        email_verified: true,
        preferred_username: `e2e-sso-${now}-${issued}`,
        name: 'E2E SSO Member',
        nonce: entry.nonce,
        iss: base,
        aud: clientId,
        iat: now,
        exp: now + 300,
      }
      return sendJSON(response, 200, {
        id_token: signIDToken(identity),
        access_token: 'e2e-access-token-not-used',
        token_type: 'Bearer',
        expires_in: 300,
      })
    }
    sendJSON(response, 404, { error: 'not_found' })
  } catch {
    if (!response.headersSent) sendJSON(response, 500, { error: 'server_error' })
  }
})

server.listen(port, '127.0.0.1', () => {
  // Playwright captures this line in its web-server log; it is not a secret.
  console.log(`fake OIDC provider listening on ${base}`)
})
