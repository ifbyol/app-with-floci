import { useCallback, useEffect, useState } from "react";
import { api, type AwsStatus, type ServiceState } from "../lib/api";

function statusBadge(s: ServiceState) {
  if (s.ready) return <span className="badge ok">{s.status}</span>;
  if (s.status === "processing" || s.status === "pending")
    return <span className="badge wait">{s.status}</span>;
  return <span className="badge bad">{s.status}</span>;
}

export default function Emulator() {
  const [status, setStatus] = useState<AwsStatus | null>(null);
  const [error, setError] = useState("");

  const load = useCallback(() => {
    api
      .awsStatus()
      .then((s) => {
        setStatus(s);
        setError("");
      })
      .catch((e: Error) => setError(e.message));
  }, []);

  useEffect(load, [load]);

  // Keep polling while anything is still coming up. Provisioning OpenSearch can
  // take minutes on a cold image cache, and watching it happen is the point.
  useEffect(() => {
    if (status?.ready) return;
    const t = setInterval(load, 3000);
    return () => clearInterval(t);
  }, [status?.ready, load]);

  return (
    <>
      <h1>Emulator</h1>
      <p className="lede">
        Every endpoint below was discovered by calling Floci&rsquo;s AWS control plane with the
        ordinary AWS SDK &mdash; the same calls this app would make against real AWS.
      </p>

      {error && (
        <p className="error" style={{ marginBottom: "1rem" }}>
          {error}
        </p>
      )}

      {status && (
        <>
          <div className="card" style={{ marginBottom: "1.25rem" }}>
            <dl className="kv">
              <dt>endpoint</dt>
              <dd className="mono">{status.flociEndpoint}</dd>
              <dt>app</dt>
              <dd>
                {status.appReady ? (
                  <span className="badge ok">connected</span>
                ) : (
                  <span className="badge wait">provisioning</span>
                )}
              </dd>
            </dl>
            {status.bootstrapError && (
              <p className="error" style={{ marginTop: ".75rem" }}>
                {status.bootstrapError}
              </p>
            )}
          </div>

          <div className="card">
            {status.services.map((s) => {
              const overridden = s.advertised !== s.effective && s.effective !== "";
              return (
                <section className="svc" key={s.service}>
                  <div className="svc-head">
                    <span className="svc-name">{s.service}</span>
                    {statusBadge(s)}
                    {overridden && <span className="badge miss">overridden</span>}
                  </div>
                  <dl className="kv">
                    <dt>call</dt>
                    <dd className="mono">{s.api}</dd>
                    <dt>resource</dt>
                    <dd className="mono">{s.resource}</dd>
                    <dt>advertised</dt>
                    <dd className="mono">{s.advertised || "—"}</dd>
                    <dt>in use</dt>
                    <dd className="mono">{s.effective || "—"}</dd>
                  </dl>
                  {s.note && <p className="svc-note">{s.note}</p>}
                </section>
              );
            })}
          </div>

          <h2>Why one of these differs</h2>
          <div className="card">
            <p className="svc-note" style={{ marginTop: 0 }}>
              RDS and ElastiCache are reached <em>through</em> Floci: it runs a TCP proxy for each,
              and the engine containers publish no ports at all. Floci builds those endpoints from
              configuration, so they can be made to match a Kubernetes Service name.
            </p>
            <p className="svc-note">
              OpenSearch works the other way round. Its container publishes its own port and Floci
              advertises the <em>Docker</em> container name, which resolves only inside Floci&rsquo;s
              Docker network. That is correct when the caller is a container on that network, and
              wrong when it is a Kubernetes pod &mdash; so in the cluster the app overrides it with
              the address the container publishes on the pod IP.
            </p>
          </div>

          <div className="row" style={{ marginTop: "1.25rem" }}>
            <button className="ghost" onClick={load}>
              Refresh
            </button>
          </div>
        </>
      )}
    </>
  );
}
