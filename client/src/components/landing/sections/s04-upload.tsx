import { copy } from "../copy";
import { MultipartUpload } from "../diagrams/multipart-upload";
import { Section } from "../section";

export function Upload() {
  return (
    <Section stop="upload" tone="ground" labelledBy="upload-title">
      <div className="grid items-center gap-12 lg:grid-cols-12 lg:gap-8">
        <div
          data-reveal="right"
          className="lg:col-span-6 lg:col-start-7 lg:row-start-1"
        >
          <h2 id="upload-title" className="t-h2">
            {copy.upload.title}
          </h2>
          <p className="t-body mt-6">{copy.upload.text}</p>
          <p className="t-body mt-6 text-muted">{copy.upload.note}</p>
        </div>
        <div
          data-reveal="left"
          className="mx-auto w-full max-w-md lg:col-span-5 lg:col-start-1 lg:row-start-1 lg:max-w-none"
        >
          <MultipartUpload />
        </div>
      </div>
    </Section>
  );
}
