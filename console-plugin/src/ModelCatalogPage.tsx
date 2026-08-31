import * as React from 'react';
import {
  K8sResourceCommon,
  useK8sWatchResource,
} from '@openshift-console/dynamic-plugin-sdk';
import { PageSection } from '@patternfly/react-core/dist/dynamic/components/Page';
import { Title } from '@patternfly/react-core/dist/dynamic/components/Title';
import { Label } from '@patternfly/react-core/dist/dynamic/components/Label';
import { Spinner } from '@patternfly/react-core/dist/dynamic/components/Spinner';
import { EmptyState, EmptyStateBody } from '@patternfly/react-core/dist/dynamic/components/EmptyState';
import { Tooltip } from '@patternfly/react-core/dist/dynamic/components/Tooltip';
import { Bullseye } from '@patternfly/react-core/dist/dynamic/layouts/Bullseye';

/*
 * Model Catalog — ONE view keyed by MODEL, reconciling the two lanes
 * a model can be exposed on:
 *
 *   - the AGENT PICKER lane (ModelConnections: what the hiring UI
 *     offers, per group), and
 *   - the GOVERNED MaaS lane (ExternalModel/MaaSModelRef behind the
 *     OpenShift AI gateway, with per-user keys and token budgets).
 *
 * The same model often lives on both, under different spellings
 * (MaaS ref names are DNS-safe: gpt-5.6-sol -> gpt-5-6-sol); this
 * page joins them on the ExternalModel's targetModel so each row is
 * the whole truth about one model. Read-only — everything renders
 * from CRs that live in git.
 */

const MAAS_NS = 'models-as-a-service';

type ModelConnection = K8sResourceCommon & {
  spec?: {
    displayName?: string;
    kind?: string;
    models?: { id: string; name?: string }[];
    access?: { groups?: string[]; users?: string[] };
  };
};
type ModelRef = K8sResourceCommon & {
  status?: { phase?: string };
  spec?: { modelRef?: { kind?: string; name?: string } };
};
type ExternalModel = K8sResourceCommon & {
  spec?: { targetModel?: string; endpoint?: string };
};
type Subscription = K8sResourceCommon & {
  spec?: {
    owner?: { groups?: { name: string }[]; users?: string[] };
    modelRefs?: { name: string; tokenRateLimits?: { limit: number; window: string }[] }[];
  };
};
type AuthPolicy = K8sResourceCommon & {
  spec?: {
    subjects?: { groups?: { name: string }[]; users?: string[] };
    modelRefs?: { name: string }[];
  };
};

const fmtLimit = (l: { limit: number; window: string }) =>
  `${l.limit >= 1000 ? `${l.limit / 1000}k` : l.limit}/${l.window}`;

const shortUser = (u: string) =>
  u.startsWith('system:serviceaccount:') ? `sa:${u.split(':').pop()}` : u;

interface Row {
  id: string; // canonical model id
  label?: string;
  pickerConns: { conn: ModelConnection; isDefault: boolean }[];
  maas?: {
    refName: string;
    ready?: string;
    budgets: { sub: string; owners: string[]; limits: string }[];
    access: { pol: string; subjects: string[] }[];
  };
  subscriptionLane: boolean;
}

const cell: React.CSSProperties = {
  padding: '10px 12px',
  borderBottom: '1px solid var(--pf-v5-global--BorderColor--100)',
  fontSize: 13,
  verticalAlign: 'top',
};

const ModelCatalogPage: React.FC = () => {
  const [conns, connsLoaded, connsErr] = useK8sWatchResource<ModelConnection[]>({
    groupVersionKind: { group: 'agentoffice.ai', version: 'v1alpha1', kind: 'ModelConnection' },
    isList: true,
  });
  const [refs] = useK8sWatchResource<ModelRef[]>({
    groupVersionKind: { group: 'maas.opendatahub.io', version: 'v1alpha1', kind: 'MaaSModelRef' },
    namespace: MAAS_NS,
    isList: true,
  });
  const [exts] = useK8sWatchResource<ExternalModel[]>({
    groupVersionKind: { group: 'maas.opendatahub.io', version: 'v1alpha1', kind: 'ExternalModel' },
    namespace: MAAS_NS,
    isList: true,
  });
  const [subs] = useK8sWatchResource<Subscription[]>({
    groupVersionKind: { group: 'maas.opendatahub.io', version: 'v1alpha1', kind: 'MaaSSubscription' },
    namespace: MAAS_NS,
    isList: true,
  });
  const [pols] = useK8sWatchResource<AuthPolicy[]>({
    groupVersionKind: { group: 'maas.opendatahub.io', version: 'v1alpha1', kind: 'MaaSAuthPolicy' },
    namespace: MAAS_NS,
    isList: true,
  });

  const rows = React.useMemo<Row[]>(() => {
    const byId = new Map<string, Row>();
    const row = (id: string): Row => {
      if (!byId.has(id))
        byId.set(id, { id, pickerConns: [], subscriptionLane: false });
      return byId.get(id)!;
    };

    for (const c of conns ?? []) {
      (c.spec?.models ?? []).forEach((m, idx) => {
        const r = row(m.id);
        if (m.name && !r.label) r.label = m.name;
        if (c.spec?.kind === 'subscription') r.subscriptionLane = true;
        else r.pickerConns.push({ conn: c, isDefault: idx === 0 });
      });
    }

    // Canonical id for a MaaS ref = its ExternalModel's targetModel.
    const target = new Map<string, string>();
    for (const e of exts ?? [])
      target.set(e.metadata?.name ?? '', e.spec?.targetModel ?? e.metadata?.name ?? '');

    for (const ref of refs ?? []) {
      const refName = ref.metadata?.name ?? '';
      const id = target.get(ref.spec?.modelRef?.name ?? refName) ?? refName;
      const r = row(id);
      const budgets = (subs ?? [])
        .map((s) => {
          const m = (s.spec?.modelRefs ?? []).find((x) => x.name === refName);
          if (!m) return undefined;
          return {
            sub: s.metadata?.name ?? '',
            owners: [
              ...(s.spec?.owner?.groups ?? []).map((g) => g.name),
              ...(s.spec?.owner?.users ?? []).map(shortUser),
            ],
            limits: (m.tokenRateLimits ?? []).map(fmtLimit).join(' + ') || 'no limit',
          };
        })
        .filter(Boolean) as Row['maas'] extends infer _T ? { sub: string; owners: string[]; limits: string }[] : never;
      const access = (pols ?? [])
        .filter((p) => (p.spec?.modelRefs ?? []).some((x) => x.name === refName))
        .map((p) => ({
          pol: p.metadata?.name ?? '',
          subjects: [
            ...(p.spec?.subjects?.groups ?? []).map((g) => g.name),
            ...(p.spec?.subjects?.users ?? []).map(shortUser),
          ],
        }));
      r.maas = { refName, ready: ref.status?.phase, budgets, access };
    }

    return [...byId.values()].sort((a, b) => a.id.localeCompare(b.id));
  }, [conns, refs, exts, subs, pols]);

  return (
    <>
      <PageSection variant="light">
        <Title headingLevel="h1">Model Catalog</Title>
        <p style={{ marginTop: 8, color: 'var(--pf-v5-global--Color--200)' }}>
          Every model on the platform, one row each, reconciled across its
          lanes: the <strong>agent picker</strong> (ModelConnections the hiring
          UI offers per group) and the <strong>governed MaaS lane</strong>{' '}
          (per-user keys and token budgets behind the OpenShift AI gateway).
          MaaS spells names DNS-safely — the row shows its ref name whenever it
          differs, so the RHOAI dashboard list maps back here.
        </p>
      </PageSection>
      <PageSection>
        {!connsLoaded && (
          <Bullseye>
            <Spinner />
          </Bullseye>
        )}
        {connsErr && <p>Failed to load: {String(connsErr)}</p>}
        {connsLoaded && rows.length === 0 && (
          <EmptyState>
            <EmptyStateBody>No models published yet.</EmptyStateBody>
          </EmptyState>
        )}
        {connsLoaded && rows.length > 0 && (
          <div style={{ overflowX: 'auto' }}>
            <table style={{ width: '100%', borderCollapse: 'collapse', background: 'var(--pf-v5-global--BackgroundColor--100)' }}>
              <thead>
                <tr>
                  {['Model', 'Lanes', 'Agent picker access', 'MaaS access & budget'].map((h) => (
                    <th key={h} style={{ ...cell, textAlign: 'left', fontSize: 12, color: 'var(--pf-v5-global--Color--200)', borderBottom: '2px solid var(--pf-v5-global--BorderColor--100)' }}>
                      {h}
                    </th>
                  ))}
                </tr>
              </thead>
              <tbody>
                {rows.map((r) => (
                  <tr key={r.id}>
                    <td style={cell}>
                      <strong>{r.id}</strong>
                      {r.label && r.label !== r.id && (
                        <div style={{ color: 'var(--pf-v5-global--Color--200)', fontSize: 12 }}>{r.label}</div>
                      )}
                      {r.maas && r.maas.refName !== r.id && (
                        <div style={{ fontSize: 11, color: 'var(--pf-v5-global--Color--200)' }}>
                          in RHOAI list as <code>{r.maas.refName}</code>
                        </div>
                      )}
                    </td>
                    <td style={cell}>
                      {r.pickerConns.map(({ conn, isDefault }) => (
                        <Label key={`pc-${conn.metadata?.name}`} color="blue" isCompact style={{ marginRight: 4, marginBottom: 2 }}>
                          picker: {conn.spec?.displayName ?? conn.metadata?.name}
                          {isDefault ? ' (default)' : ''}
                        </Label>
                      ))}
                      {r.maas && (
                        <Label
                          color={r.maas.ready === 'Ready' ? 'green' : 'orange'}
                          isCompact
                          style={{ marginRight: 4, marginBottom: 2 }}
                        >
                          MaaS {r.maas.ready === 'Ready' ? '' : `(${r.maas.ready ?? 'pending'})`}
                        </Label>
                      )}
                      {r.subscriptionLane && (
                        <Label color="purple" isCompact style={{ marginRight: 4, marginBottom: 2 }}>
                          subscription lane
                        </Label>
                      )}
                      {!r.maas && r.pickerConns.length > 0 && (
                        <div style={{ fontSize: 11, color: 'var(--pf-v5-global--Color--200)' }}>picker only — not MaaS-governed</div>
                      )}
                      {r.maas && r.pickerConns.length === 0 && !r.subscriptionLane && (
                        <div style={{ fontSize: 11, color: 'var(--pf-v5-global--Color--200)' }}>governed only — not on the hiring menu</div>
                      )}
                    </td>
                    <td style={cell}>
                      {r.pickerConns.length === 0 && <span style={{ color: 'var(--pf-v5-global--Color--200)' }}>—</span>}
                      {Array.from(
                        new Set(
                          r.pickerConns.flatMap(({ conn }) => [
                            ...(conn.spec?.access?.groups ?? []).map((g) => `g:${g}`),
                            ...(conn.spec?.access?.users ?? []).map((u) => `u:${u}`),
                          ]),
                        ),
                      ).map((s) => (
                        <Label key={`pa-${r.id}-${s}`} color={s.startsWith('g:') ? 'blue' : 'purple'} isCompact style={{ marginRight: 4, marginBottom: 2 }}>
                          {s.slice(2)}
                        </Label>
                      ))}
                    </td>
                    <td style={cell}>
                      {!r.maas && <span style={{ color: 'var(--pf-v5-global--Color--200)' }}>—</span>}
                      {r.maas &&
                        r.maas.budgets.map((b) => (
                          <Tooltip key={`bt-${r.id}-${b.sub}`} content={`subscription ${b.sub}`}>
                            <Label color="gold" isCompact style={{ marginRight: 4, marginBottom: 2 }}>
                              {b.owners.join(', ') || b.sub} · {b.limits}
                            </Label>
                          </Tooltip>
                        ))}
                      {r.maas && r.maas.budgets.length === 0 && (
                        <Label color="red" isCompact style={{ marginRight: 4 }}>
                          no subscription
                        </Label>
                      )}
                      {r.maas && r.maas.access.length === 0 && (
                        <Label color="red" isCompact>
                          no auth policy — 403
                        </Label>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </PageSection>
    </>
  );
};

export default ModelCatalogPage;
