// Pi extension: registers an OpenAI-compatible provider named "litellm" that routes to a
// LiteLLM proxy. The base URL, API key file path, and model id are supplied by the bridge
// via the PI_LITELLM_BASE_URL, PI_LITELLM_API_KEY_FILE, and PI_LITELLM_MODEL environment
// variables.
//
// The API key is read directly from the mounted Secret file inside `resolve()` at request
// time (cached for process lifetime). This keeps the key out of argv and the persistent
// process environment — it only ever lives in the in-memory resolved auth object — while
// honoring projected-Secret rotation on pod restart.
//
// The model catalog is intentionally a single entry: LiteLLM is a passthrough proxy whose
// model list is dynamic and per-credential, so Pi cannot enumerate it up front. The bridge
// pins the one model to use, and Pi's --provider/--model resolution needs at least one
// registered model to recognize the provider.
//
// See https://pi.dev/docs/latest/custom-provider for the provider registration API.

import { readFileSync } from "node:fs";
import { createProvider, lazyApi } from "@earendil-works/pi-ai";
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";

export default function (pi: ExtensionAPI) {
  const baseUrl = process.env.PI_LITELLM_BASE_URL;
  if (!baseUrl) {
    // Without a base URL the provider is inert; Pi will fall back to other configured
    // providers. This keeps the extension safe to load in any worker image.
    return;
  }
  const keyFile = process.env.PI_LITELLM_API_KEY_FILE || "/secrets/litellm/api-key";
  const modelId = process.env.PI_LITELLM_MODEL || "glm-5.2";

  let cachedKey: string | undefined;
  const resolveKey = (): string => {
    if (cachedKey !== undefined) {
      return cachedKey;
    }
    const raw = readFileSync(keyFile, "utf8").trim();
    cachedKey = raw || undefined;
    return cachedKey;
  };

  pi.registerProvider(
    createProvider({
      id: "litellm",
      name: "LiteLLM Proxy",
      baseUrl,
      auth: {
        apiKey: {
          name: "LiteLLM API key",
          // Read the key from the projected Secret file at request time. Returning undefined
          // here makes the provider report as unconfigured, which surfaces a clear error.
          async resolve() {
            const key = resolveKey();
            if (!key) {
              return undefined;
            }
            return {
              auth: { apiKey: key },
              source: "mounted key file",
            };
          },
        },
      },
      models: [
        {
          id: modelId,
          name: modelId,
          api: "openai-completions",
          provider: "litellm",
          baseUrl,
          reasoning: false,
          input: ["text"],
          cost: { input: 0, output: 0 },
          contextWindow: 128000,
          maxTokens: 16384,
        },
      ],
      // pi-ai 0.85.x removed the `openAICompletionsApi()` factory in favor of lazy-loaded
      // API modules. The OpenAI-completions implementation module exports `stream` and
      // `streamSimple`, which is exactly the `ProviderStreams` shape `createProvider` expects.
      // `lazyApi` defers the dynamic import until the first model call, so a load failure
      // surfaces as a stream error rather than failing the whole extension at startup.
      api: lazyApi(() => import("@earendil-works/pi-ai/api/openai-completions")),
    }),
  );
}
