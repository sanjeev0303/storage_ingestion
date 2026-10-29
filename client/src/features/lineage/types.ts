// OBJECT_VERSION is reserved by the server and never appears in a response.
export type NodeKind =
  | "DATASET"
  | "DATASET_VERSION"
  | "MANIFEST_IMPORT"
  | "VALIDATION_REPORT"
  | "TRANSFORMATION"
  | "OBJECT_VERSION"
  | "TRAINING_RUN"
  | "MODEL";

export type Relation =
  | "CONTAINS"
  | "DERIVED_FROM"
  | "IMPORTED_FROM"
  | "VALIDATED_BY"
  | "CREATED_FROM_SNAPSHOT"
  | "TRANSFORMED_BY"
  | "TRAINED_ON"
  | "PRODUCED_BY";

export type Direction = "upstream" | "downstream";

export type Ref = { kind: NodeKind; id: string };

export type Edge = {
  from: Ref;
  to: Ref;
  relation: Relation;
  // Distance of the far end of the edge from the start node.
  depth: number;
  actor: string;
  job_id?: string;
  details?: Record<string, unknown>;
  created_at: string;
};

export type LineageResponse = {
  start: Ref;
  direction: Direction;
  edges: Edge[];
  next_cursor?: string;
  truncated: boolean;
};

export type LineageQuery = {
  datasetId: string;
  versionId?: string;
  direction: Direction;
  depth: number;
};
