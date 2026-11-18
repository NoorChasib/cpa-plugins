import { useId, useState } from "react"

import { APICreditsCard } from "./APICredits"
import { SectionHead } from "./ProviderSection"
import { OPENAI_COPY } from "../lib/apicredits"
import { CARD, cardKey, openedKeys, setOpened } from "../lib/collapsed"
import { quotaCacheURL } from "../lib/session"
import type { APICredits } from "../lib/types"

/**
 * The OpenAI API credit's section, and the first half of its card's fold key,
 * "openai-api:@api-credits". No provider in the document has this id, so the
 * fold is the card's alone.
 */
export const OPENAI_SECTION = "openai-api"

/**
 * The monthly OpenAI API credit a ChatGPT Pro or Business plan grants one
 * OpenAI API organization, in a section of its own below the quota providers.
 *
 * Never under Codex. Codex's section is ChatGPT sign-in subscriptions, which
 * this credit is not, and OpenAI does not cover Codex usage through API keys
 * with it, so it belongs to no provider shown above. It is the Claude card in
 * OpenAI's words, linking to the OpenAI section of Quota Cache's page, where
 * its values are entered. Nothing when Quota Cache publishes no OpenAI credit.
 */
export function OpenAICreditsSection({ credits }: { credits: APICredits | null }) {
  const headingID = useId()
  // Read once at mount, as a provider section reads its folds.
  const [opened, setOpenedKeys] = useState(openedKeys)
  if (!credits) return null
  const key = cardKey(OPENAI_SECTION, CARD.apiCredits)

  return (
    <section className="qg-prov" aria-labelledby={headingID}>
      <SectionHead id={headingID} title="OpenAI API" />
      <APICreditsCard
        credits={credits}
        copy={OPENAI_COPY}
        link={quotaCacheURL("openai-api-credits")}
        open={opened.has(key)}
        onToggle={() => setOpenedKeys(setOpened(key, !opened.has(key)))}
      />
    </section>
  )
}
