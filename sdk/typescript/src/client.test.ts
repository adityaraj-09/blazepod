// Layer: TypeScript SDK — unit tests for the SandockClient.
// Tests type safety and basic client construction without making real HTTP calls.
import { SandockClient, SandockAPIError } from "./client.js";

// Node 18+ has native fetch; these tests use a mock to avoid network calls.
async function testClientConstruction() {
  const client = new SandockClient({
    baseURL: "http://localhost:8080",
    apiKey: "testtoken123",
    timeoutMs: 5000,
  });

  // Verify the client is constructed correctly.
  if (!(client instanceof SandockClient)) {
    throw new Error("client should be an instance of SandockClient");
  }
  console.log("✓ SandockClient construction");
}

async function testSandockAPIError() {
  const err = new SandockAPIError(429, "quota exceeded");
  if (err.statusCode !== 429) throw new Error("statusCode mismatch");
  if (!err.message.includes("429")) throw new Error("message should include status code");
  if (err.name !== "SandockAPIError") throw new Error("name mismatch");
  console.log("✓ SandockAPIError");
}

// Run all tests.
(async () => {
  try {
    await testClientConstruction();
    await testSandockAPIError();
    console.log("\nAll TypeScript SDK tests passed.");
  } catch (e) {
    console.error("FAIL:", e);
    process.exit(1);
  }
})();
