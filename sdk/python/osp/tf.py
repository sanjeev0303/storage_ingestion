"""A tf.data.Dataset over one pinned dataset snapshot (osp.stream).

    from osp.stream import SnapshotStream
    from osp.tf import as_tf_dataset
    stream = SnapshotStream(client, dataset_id, version_id, split="train", cache_dir="/data/cache")
    ds = as_tf_dataset(stream, output_signature=(tf.TensorSpec((), tf.string), tf.TensorSpec((), tf.string)),
                       transform=lambda s: (s["data"], s["label"]))
    ds = ds.batch(32).prefetch(tf.data.AUTOTUNE)

With several input pipelines (tf.distribute), give each its
input_pipeline_id and num_input_pipelines as shard and shards, so every
member is read by exactly one of them.
"""


def as_tf_dataset(stream, output_signature, transform=None, kind="files", shard=0, shards=1):
    try:
        import tensorflow as tf
    except ImportError:
        raise ImportError("osp.tf needs TensorFlow: pip install tensorflow") from None

    def generate():
        yield from stream.samples(kind, shard, shards, transform)

    return tf.data.Dataset.from_generator(generate, output_signature=output_signature)
