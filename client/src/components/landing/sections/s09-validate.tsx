import { Section } from "../section";
import { ValidateBody } from "./validate-body";

export function Validate() {
  return (
    <Section stop="validate" tone="ground" labelledBy="validate-title">
      <ValidateBody />
    </Section>
  );
}
