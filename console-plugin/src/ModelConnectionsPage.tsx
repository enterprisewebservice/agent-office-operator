import * as React from 'react';
import {
  K8sModel,
  K8sResourceCommon,
  consoleFetch,
  k8sCreate,
  k8sDelete,
  k8sGet,
  k8sUpdate,
  useK8sWatchResource,
} from '@openshift-console/dynamic-plugin-sdk';
import { PageSection } from '@patternfly/react-core/dist/dynamic/components/Page';
import { Title } from '@patternfly/react-core/dist/dynamic/components/Title';
import { Card, CardBody, CardHeader, CardTitle } from '@patternfly/react-core/dist/dynamic/components/Card';
import { Label } from '@patternfly/react-core/dist/dynamic/components/Label';
import { Spinner } from '@patternfly/react-core/dist/dynamic/components/Spinner';
import { EmptyState, EmptyStateBody } from '@patternfly/react-core/dist/dynamic/components/EmptyState';
import { Button } from '@patternfly/react-core/dist/dynamic/components/Button';
import { Modal, ModalVariant } from '@patternfly/react-core/dist/dynamic/components/Modal';
import { Form, FormGroup, FormHelperText } from '@patternfly/react-core/dist/dynamic/components/Form';
import { TextInput } from '@patternfly/react-core/dist/dynamic/components/TextInput';
import { FormSelect, FormSelectOption } from '@patternfly/react-core/dist/dynamic/components/FormSelect';
import { Radio } from '@patternfly/react-core/dist/dynamic/components/Radio';
import { Checkbox } from '@patternfly/react-core/dist/dynamic/components/Checkbox';
import { Alert } from '@patternfly/react-core/dist/dynamic/components/Alert';
import { HelperText, HelperTextItem } from '@patternfly/react-core/dist/dynamic/components/HelperText';
import {
  Select,
  SelectOption,
  SelectVariant,
} from '@patternfly/react-core/dist/dynamic/deprecated/components/Select';
import { Bullseye } from '@patternfly/react-core/dist/dynamic/layouts/Bullseye';
import { Flex, FlexItem } from '@patternfly/react-core/dist/dynamic/layouts/Flex';
import { Gallery } from '@patternfly/react-core/dist/dynamic/layouts/Gallery';
import PlusCircleIcon from '@patternfly/react-icons/dist/dynamic/icons/plus-circle-icon';
import TrashIcon from '@patternfly/react-icons/dist/dynamic/icons/trash-icon';
import PencilAltIcon from '@patternfly/react-icons/dist/dynamic/icons/pencil-alt-icon';
import TimesIcon from '@patternfly/react-icons/dist/dynamic/icons/times-icon';
import SearchIcon from '@patternfly/react-icons/dist/dynamic/icons/search-icon';

/*
 * Model Connections — the admin side of "publish brains, don't hand
 * out keys".
 *
 * Design rule: nothing is free-typed when the platform can offer the
 * real values —
 *   - groups/users: multi-selects fed by the cluster's Groups/Users
 *     UNION whatever existing connections already grant (that union
 *     is what covers identities that live only in the SSO provider,
 *     e.g. Developer Hub's `attendees`), with a guarded "add" for a
 *     new SSO-only name;
 *   - models: fetched from the endpoint's own /v1/models (via the
 *     operator's probe) and offered as checkboxes; subscription
 *     routes get one-click chips for the platform's known models;
 *   - endpoint URL: a picker over discovered in-cluster serving
 *     endpoints (KServe InferenceServices + URLs already published),
 *     with Custom as the deliberate escape hatch;
 *   - API dialect / key strategy / kind: fixed vocabularies, so
 *     selects and radios.
 *
 * The API key stays write-only: typed once, stored as a Secret in
 * agent-office, never read back or displayed.
 */

type ModelEntry = { id: string; name?: string };

type ModelConnection = K8sResourceCommon & {
  spec?: {
    displayName?: string;
    description?: string;
    kind?: 'subscription' | 'apiKey' | 'endpoint';
    provider?: string;
    baseUrl?: string;
    api?: string;
    apiKeySecretRef?: { name?: string; namespace?: string; key?: string };
    models?: ModelEntry[];
    keyStrategy?: string;
    access?: { groups?: string[]; users?: string[] };
  };
};

type Named = K8sResourceCommon;
type InferenceService = K8sResourceCommon & { status?: { url?: string } };

const ADMIN_NS = 'agent-office';
const PROBE_URL =
  '/api/proxy/plugin/agent-office-plugin/catalog/catalog/model-connections/probe';

// The subscription-side model catalog the platform ships today.
const KNOWN_SUBSCRIPTION_MODELS: ModelEntry[] = [
  { id: 'gpt-5.6-sol', name: 'GPT-5.6 Sol' },
  { id: 'gpt-5.6', name: 'GPT-5.6' },
  { id: 'gpt-5.5', name: 'GPT-5.5' },
  { id: 'gpt-5.4', name: 'GPT-5.4' },
];

const API_DIALECTS = ['openai-completions', 'openai-chatgpt-responses'];

const ModelConnectionModel = {
  apiGroup: 'agentoffice.ai',
  apiVersion: 'v1alpha1',
  kind: 'ModelConnection',
  plural: 'modelconnections',
  abbr: 'MC',
  label: 'ModelConnection',
  labelPlural: 'ModelConnections',
  namespaced: false,
} as K8sModel;

const SecretModel = {
  apiVersion: 'v1',
  kind: 'Secret',
  plural: 'secrets',
  abbr: 'S',
  label: 'Secret',
  labelPlural: 'Secrets',
  namespaced: true,
} as K8sModel;

const kindColor = (k?: string) =>
  k === 'endpoint' ? 'blue' : k === 'subscription' ? 'purple' : 'gold';

const kindHelp: Record<string, string> = {
  endpoint:
    'Any OpenAI-compatible URL — MaaS, LiteLLM, vLLM. The operator renders it onto every gateway whose agents pick it and projects the key; agents never see credentials.',
  subscription:
    'A consumer-subscription route (ChatGPT/Codex). The gateway’s stored OAuth pays the bill; this entry just controls who may pick it.',
  apiKey:
    'The metered first-party API route (api.openai.com). Billing rides the gateway’s existing key; this entry controls who may pick it.',
};

const slugify = (s: string) =>
  s
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, '-')
    .replace(/^-+|-+$/g, '')
    .slice(0, 63);

/** Checkbox multi-select over a known vocabulary, with a guarded
 *  "add another" for names that only exist in the SSO provider. */
const MultiPicker: React.FC<{
  id: string;
  options: string[];
  value: string[];
  onChange: (next: string[]) => void;
  placeholder: string;
  addHint: string;
}> = ({ id, options, value, onChange, placeholder, addHint }) => {
  const [open, setOpen] = React.useState(false);
  const all = React.useMemo(
    () => Array.from(new Set([...options, ...value])).sort(),
    [options, value],
  );
  return (
    <Select
      id={id}
      variant={SelectVariant.checkbox}
      isOpen={open}
      onToggle={(_e: unknown, o: boolean) => setOpen(o)}
      selections={value}
      onSelect={(_e: unknown, sel: unknown) => {
        const s = String(sel);
        onChange(value.includes(s) ? value.filter((v) => v !== s) : [...value, s]);
      }}
      placeholderText={placeholder}
      isCreatable
      createText={addHint}
      onCreateOption={(newVal: string) => {
        const s = newVal.trim();
        if (s && !value.includes(s)) onChange([...value, s]);
      }}
      maxHeight={260}
    >
      {all.map((o) => (
        <SelectOption key={o} value={o} />
      ))}
    </Select>
  );
};

interface FormState {
  name: string;
  nameTouched: boolean;
  displayName: string;
  description: string;
  kind: 'endpoint' | 'subscription' | 'apiKey';
  provider: string;
  baseUrl: string;
  baseUrlCustom: boolean;
  api: string;
  models: ModelEntry[];
  groups: string[];
  users: string[];
  keyStrategy: string;
  apiKeyInput: string; // write-only
}

const emptyForm = (): FormState => ({
  name: '',
  nameTouched: false,
  displayName: '',
  description: '',
  kind: 'endpoint',
  provider: 'openai-codex',
  baseUrl: '',
  baseUrlCustom: false,
  api: 'openai-completions',
  models: [],
  groups: [],
  users: [],
  keyStrategy: 'shared',
  apiKeyInput: '',
});

const formFrom = (c: ModelConnection): FormState => ({
  name: c.metadata?.name ?? '',
  nameTouched: true,
  displayName: c.spec?.displayName ?? '',
  description: c.spec?.description ?? '',
  kind: (c.spec?.kind as FormState['kind']) ?? 'endpoint',
  provider: c.spec?.provider ?? 'openai-codex',
  baseUrl: c.spec?.baseUrl ?? '',
  baseUrlCustom: false,
  api: c.spec?.api ?? 'openai-completions',
  models: c.spec?.models ?? [],
  groups: c.spec?.access?.groups ?? [],
  users: c.spec?.access?.users ?? [],
  keyStrategy: c.spec?.keyStrategy ?? 'shared',
  apiKeyInput: '',
});

const ModelConnectionsPage: React.FC = () => {
  const [conns, loaded, loadError] = useK8sWatchResource<ModelConnection[]>({
    groupVersionKind: { group: 'agentoffice.ai', version: 'v1alpha1', kind: 'ModelConnection' },
    isList: true,
  });
  const [secrets] = useK8sWatchResource<Named[]>({
    groupVersionKind: { version: 'v1', kind: 'Secret' },
    namespace: ADMIN_NS,
    isList: true,
  });
  const [clusterGroups] = useK8sWatchResource<Named[]>({
    groupVersionKind: { group: 'user.openshift.io', version: 'v1', kind: 'Group' },
    isList: true,
  });
  const [clusterUsers] = useK8sWatchResource<Named[]>({
    groupVersionKind: { group: 'user.openshift.io', version: 'v1', kind: 'User' },
    isList: true,
  });
  // Discovered in-cluster model servers (tolerate the CRD being absent).
  const [inferenceServices] = useK8sWatchResource<InferenceService[]>({
    groupVersionKind: { group: 'serving.kserve.io', version: 'v1beta1', kind: 'InferenceService' },
    isList: true,
  });

  const secretNames = React.useMemo(
    () => new Set((secrets ?? []).map((s) => s.metadata?.name)),
    [secrets],
  );

  // Vocabularies = live cluster objects ∪ names existing connections
  // already use (covers SSO-only identities like `attendees`).
  const groupOptions = React.useMemo(() => {
    const s = new Set<string>((clusterGroups ?? []).map((g) => g.metadata?.name || ''));
    (conns ?? []).forEach((c) => (c.spec?.access?.groups ?? []).forEach((g) => s.add(g)));
    s.delete('');
    return Array.from(s).sort();
  }, [clusterGroups, conns]);
  const userOptions = React.useMemo(() => {
    const s = new Set<string>((clusterUsers ?? []).map((u) => u.metadata?.name || ''));
    (conns ?? []).forEach((c) => (c.spec?.access?.users ?? []).forEach((u) => s.add(u)));
    s.delete('');
    return Array.from(s).sort();
  }, [clusterUsers, conns]);
  const endpointOptions = React.useMemo(() => {
    const s = new Set<string>();
    (inferenceServices ?? []).forEach((i) => {
      if (i.status?.url) s.add(`${i.status.url.replace(/\/$/, '')}/v1`);
    });
    (conns ?? []).forEach((c) => {
      if (c.spec?.kind === 'endpoint' && c.spec?.baseUrl) s.add(c.spec.baseUrl);
    });
    return Array.from(s).sort();
  }, [inferenceServices, conns]);

  const [editing, setEditing] = React.useState<ModelConnection | 'new' | null>(null);
  const [form, setForm] = React.useState<FormState>(emptyForm());
  const [busy, setBusy] = React.useState(false);
  const [formError, setFormError] = React.useState<string | undefined>();
  const [deleting, setDeleting] = React.useState<ModelConnection | null>(null);
  const [deleteSecretToo, setDeleteSecretToo] = React.useState(true);

  // Endpoint model discovery.
  const [probing, setProbing] = React.useState(false);
  const [probeError, setProbeError] = React.useState<string | undefined>();
  const [probed, setProbed] = React.useState<string[] | undefined>();

  const patch = (p: Partial<FormState>) => setForm((f) => ({ ...f, ...p }));

  const openCreate = () => {
    setForm(emptyForm());
    setFormError(undefined);
    setProbed(undefined);
    setProbeError(undefined);
    setEditing('new');
  };
  const openEdit = (c: ModelConnection) => {
    setForm(formFrom(c));
    setFormError(undefined);
    setProbed(undefined);
    setProbeError(undefined);
    setEditing(c);
  };

  const effectiveName = form.nameTouched || form.name ? form.name : slugify(form.displayName);

  // Where the key lives: respect an existing connection's own ref;
  // only fresh connections get the managed `<name>-key` convention.
  const keyRef = (): { name: string; namespace: string; key: string } => {
    const existing = editing !== 'new' && editing ? editing.spec?.apiKeySecretRef : undefined;
    if (existing?.name) {
      return {
        name: existing.name,
        namespace: existing.namespace || ADMIN_NS,
        key: existing.key || 'api-key',
      };
    }
    return { name: `${effectiveName}-key`, namespace: ADMIN_NS, key: 'api-key' };
  };

  const fetchModels = async () => {
    setProbing(true);
    setProbeError(undefined);
    try {
      const body: Record<string, unknown> = { baseUrl: form.baseUrl.trim() };
      if (form.apiKeyInput) body.apiKey = form.apiKeyInput;
      else {
        const ref = keyRef();
        if (secretNames.has(ref.name)) body.secretRef = ref;
      }
      const res = await consoleFetch(PROBE_URL, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });
      const data = await res.json();
      if (!res.ok) throw new Error(data?.error || `HTTP ${res.status}`);
      const ids: string[] = (data.models ?? []).map((m: ModelEntry) => m.id);
      setProbed(ids);
      if (ids.length === 0) setProbeError('The endpoint answered but lists no models.');
    } catch (e) {
      setProbed(undefined);
      setProbeError((e as Error).message);
    } finally {
      setProbing(false);
    }
  };

  const toggleModel = (id: string) => {
    setForm((f) => ({
      ...f,
      models: f.models.some((m) => m.id === id)
        ? f.models.filter((m) => m.id !== id)
        : [...f.models, { id }],
    }));
  };
  const setModelLabel = (id: string, name: string) => {
    setForm((f) => ({
      ...f,
      models: f.models.map((m) => (m.id === id ? { ...m, name: name || undefined } : m)),
    }));
  };

  const validate = (): string | undefined => {
    if (!form.displayName.trim()) return 'Display name is required.';
    if (!effectiveName || !/^[a-z0-9]([a-z0-9-]*[a-z0-9])?$/.test(effectiveName))
      return 'Name must be lowercase letters, digits and dashes.';
    if (form.kind === 'endpoint') {
      if (!/^https?:\/\//.test(form.baseUrl.trim()))
        return 'Pick or enter an http(s) endpoint URL.';
      const ref = keyRef();
      if (editing === 'new' && !form.apiKeyInput && !secretNames.has(ref.name))
        return `Enter an API key, or create Secret ${ADMIN_NS}/${ref.name} first.`;
    }
    if (form.models.length === 0)
      return form.kind === 'endpoint'
        ? 'Fetch the endpoint’s models and check at least one.'
        : 'Add at least one model.';
    if (form.groups.length === 0 && form.users.length === 0)
      return 'Grant access to at least one group or user, or nobody will see it.';
    return undefined;
  };

  const save = async () => {
    const err = validate();
    if (err) {
      setFormError(err);
      return;
    }
    setBusy(true);
    setFormError(undefined);
    try {
      const ref = keyRef();
      if (form.kind === 'endpoint' && form.apiKeyInput) {
        let existing: any;
        try {
          existing = await k8sGet({ model: SecretModel, name: ref.name, ns: ref.namespace });
        } catch (_e) {
          existing = undefined;
        }
        if (existing) {
          existing.stringData = { ...(existing.stringData ?? {}), [ref.key]: form.apiKeyInput };
          await k8sUpdate({ model: SecretModel, data: existing });
        } else {
          await k8sCreate({
            model: SecretModel,
            data: {
              apiVersion: 'v1',
              kind: 'Secret',
              metadata: { name: ref.name, namespace: ref.namespace },
              type: 'Opaque',
              stringData: { [ref.key]: form.apiKeyInput },
            } as any,
          });
        }
      }

      const spec: ModelConnection['spec'] = {
        displayName: form.displayName.trim(),
        ...(form.description.trim() ? { description: form.description.trim() } : {}),
        kind: form.kind,
        ...(form.kind !== 'endpoint' ? { provider: form.provider } : {}),
        ...(form.kind === 'endpoint'
          ? {
              baseUrl: form.baseUrl.trim(),
              api: form.api,
              apiKeySecretRef: ref,
            }
          : {}),
        models: form.models,
        keyStrategy: form.keyStrategy,
        access: {
          ...(form.groups.length ? { groups: form.groups } : {}),
          ...(form.users.length ? { users: form.users } : {}),
        },
      };

      if (editing === 'new') {
        await k8sCreate({
          model: ModelConnectionModel,
          data: {
            apiVersion: 'agentoffice.ai/v1alpha1',
            kind: 'ModelConnection',
            metadata: { name: effectiveName },
            spec,
          } as any,
        });
      } else if (editing) {
        const fresh: any = await k8sGet({
          model: ModelConnectionModel,
          name: editing.metadata!.name!,
        });
        fresh.spec = spec;
        await k8sUpdate({ model: ModelConnectionModel, data: fresh });
      }
      setEditing(null);
    } catch (e) {
      setFormError((e as Error).message);
    } finally {
      setBusy(false);
    }
  };

  const doDelete = async () => {
    if (!deleting) return;
    setBusy(true);
    try {
      const ref = deleting.spec?.apiKeySecretRef;
      await k8sDelete({ model: ModelConnectionModel, resource: deleting as any });
      if (
        deleteSecretToo &&
        deleting.spec?.kind === 'endpoint' &&
        ref?.name &&
        secretNames.has(ref.name)
      ) {
        await k8sDelete({
          model: SecretModel,
          resource: {
            metadata: { name: ref.name, namespace: ref.namespace || ADMIN_NS },
          } as any,
        });
      }
      setDeleting(null);
    } catch (e) {
      setFormError((e as Error).message);
    } finally {
      setBusy(false);
    }
  };

  const canProbe =
    /^https?:\/\//.test(form.baseUrl.trim()) &&
    (!!form.apiKeyInput || secretNames.has(keyRef().name));

  return (
    <>
      <PageSection variant="light">
        <Flex>
          <FlexItem>
            <Title headingLevel="h1">Model Connections</Title>
            <p style={{ marginTop: 8, color: 'var(--pf-v5-global--Color--200)' }}>
              The brains this platform publishes. Each connection is a model route
              with an access list — the hiring UI shows people only the
              connections you granted their group. Keys live in the{' '}
              <code>{ADMIN_NS}</code> namespace and are projected onto gateways by
              the operator; agents and attendees never see them.
            </p>
          </FlexItem>
          <FlexItem align={{ default: 'alignRight' }}>
            <Button variant="primary" icon={<PlusCircleIcon />} onClick={openCreate}>
              Publish a connection
            </Button>
          </FlexItem>
        </Flex>
      </PageSection>
      <PageSection>
        {!loaded && (
          <Bullseye>
            <Spinner />
          </Bullseye>
        )}
        {loadError && <p>Failed to load: {String(loadError)}</p>}
        {loaded && conns?.length === 0 && (
          <EmptyState>
            <EmptyStateBody>
              Nothing published yet. Publish a connection to put a brain on the
              hiring menu.
            </EmptyStateBody>
          </EmptyState>
        )}
        {loaded && conns && conns.length > 0 && (
          <Gallery hasGutter minWidths={{ default: '340px' }}>
            {conns.map((c) => {
              const ref = c.spec?.apiKeySecretRef;
              const isEndpoint = c.spec?.kind === 'endpoint';
              const keyOk = !isEndpoint || (ref?.name ? secretNames.has(ref.name) : false);
              const groups = c.spec?.access?.groups ?? [];
              const users = c.spec?.access?.users ?? [];
              return (
                <Card key={c.metadata?.uid} isCompact isFullHeight>
                  <CardHeader>
                    <CardTitle>
                      <Flex>
                        <FlexItem>
                          <strong>{c.spec?.displayName || c.metadata?.name}</strong>
                        </FlexItem>
                        <FlexItem align={{ default: 'alignRight' }}>
                          <Label color={kindColor(c.spec?.kind)}>{c.spec?.kind}</Label>
                        </FlexItem>
                      </Flex>
                    </CardTitle>
                  </CardHeader>
                  <CardBody>
                    {c.spec?.description && (
                      <p style={{ marginBottom: 8, color: 'var(--pf-v5-global--Color--200)' }}>
                        {c.spec.description}
                      </p>
                    )}
                    {isEndpoint && (
                      <p style={{ marginBottom: 4, fontSize: 13, wordBreak: 'break-all' }}>
                        <strong>endpoint:</strong> {c.spec?.baseUrl}
                      </p>
                    )}
                    {!isEndpoint && c.spec?.provider && (
                      <p style={{ marginBottom: 4, fontSize: 13 }}>
                        <strong>provider:</strong> {c.spec.provider}
                      </p>
                    )}
                    <p style={{ marginBottom: 4, fontSize: 13 }}>
                      <strong>models:</strong>{' '}
                      {(c.spec?.models ?? []).map((m) => (
                        <Label key={m.id} isCompact style={{ marginRight: 4 }}>
                          {m.name || m.id}
                        </Label>
                      ))}
                    </p>
                    <p style={{ marginBottom: 4, fontSize: 13 }}>
                      <strong>visible to:</strong>{' '}
                      {groups.map((g) => (
                        <Label key={`g-${g}`} color="blue" isCompact style={{ marginRight: 4 }}>
                          group: {g}
                        </Label>
                      ))}
                      {users.map((u) => (
                        <Label key={`u-${u}`} color="purple" isCompact style={{ marginRight: 4 }}>
                          {u}
                        </Label>
                      ))}
                      {groups.length === 0 && users.length === 0 && (
                        <Label color="grey" isCompact>
                          nobody (hidden)
                        </Label>
                      )}
                    </p>
                    {isEndpoint && (
                      <p style={{ marginBottom: 4, fontSize: 13 }}>
                        <strong>key:</strong>{' '}
                        <Label color={keyOk ? 'green' : 'red'} isCompact>
                          {keyOk ? `stored (${ref?.name})` : ref?.name ? `missing: ${ref.name}` : 'none'}
                        </Label>
                      </p>
                    )}
                    <Flex style={{ marginTop: 12 }}>
                      <FlexItem>
                        <Button variant="secondary" icon={<PencilAltIcon />} onClick={() => openEdit(c)}>
                          Edit
                        </Button>
                      </FlexItem>
                      <FlexItem>
                        <Button
                          variant="danger"
                          icon={<TrashIcon />}
                          onClick={() => {
                            setDeleteSecretToo(true);
                            setDeleting(c);
                          }}
                        >
                          Unpublish
                        </Button>
                      </FlexItem>
                    </Flex>
                  </CardBody>
                </Card>
              );
            })}
          </Gallery>
        )}
      </PageSection>

      {/* Publish / edit */}
      <Modal
        variant={ModalVariant.medium}
        title={editing === 'new' ? 'Publish a connection' : `Edit ${form.displayName || form.name}`}
        isOpen={editing !== null}
        onClose={() => setEditing(null)}
        actions={[
          <Button key="save" variant="primary" onClick={save} isLoading={busy} isDisabled={busy}>
            {editing === 'new' ? 'Publish' : 'Save'}
          </Button>,
          <Button key="cancel" variant="link" onClick={() => setEditing(null)}>
            Cancel
          </Button>,
        ]}
      >
        <Form>
          {formError && <Alert variant="danger" isInline title={formError} />}
          <FormGroup label="What kind of route is this?" isRequired>
            {(['endpoint', 'subscription', 'apiKey'] as const).map((k) => (
              <Radio
                key={k}
                id={`kind-${k}`}
                name="kind"
                isChecked={form.kind === k}
                onChange={() => patch({ kind: k, models: [] })}
                isDisabled={editing !== 'new'}
                label={
                  k === 'endpoint'
                    ? 'OpenAI-compatible endpoint'
                    : k === 'subscription'
                      ? 'Subscription (ChatGPT/Codex)'
                      : 'Metered API preset'
                }
                description={kindHelp[k]}
              />
            ))}
          </FormGroup>
          <FormGroup label="Display name" isRequired>
            <TextInput
              value={form.displayName}
              onChange={(_e, v) => patch({ displayName: v })}
              placeholder="Models-as-a-Service"
            />
          </FormGroup>
          <FormGroup label="Name">
            <TextInput
              value={effectiveName}
              onChange={(_e, v) => patch({ name: slugify(v), nameTouched: true })}
              isDisabled={editing !== 'new'}
            />
            <FormHelperText>
              <HelperText>
                <HelperTextItem>
                  Becomes the provider id agents reference. Fixed after publish.
                </HelperTextItem>
              </HelperText>
            </FormHelperText>
          </FormGroup>
          <FormGroup label="Description">
            <TextInput
              value={form.description}
              onChange={(_e, v) => patch({ description: v })}
              placeholder="One line shown under the label on the picker"
            />
          </FormGroup>
          {form.kind !== 'endpoint' && (
            <FormGroup label="Provider preset" isRequired>
              <FormSelect value={form.provider} onChange={(_e, v) => patch({ provider: v })}>
                <FormSelectOption value="openai-codex" label="openai-codex (ChatGPT/Codex subscription)" />
                <FormSelectOption value="openai" label="openai (metered api.openai.com)" />
              </FormSelect>
            </FormGroup>
          )}
          {form.kind === 'endpoint' && (
            <>
              <FormGroup label="Endpoint" isRequired>
                <FormSelect
                  value={form.baseUrlCustom || !endpointOptions.includes(form.baseUrl) && form.baseUrl ? '__custom__' : form.baseUrl}
                  onChange={(_e, v) =>
                    v === '__custom__'
                      ? patch({ baseUrlCustom: true })
                      : patch({ baseUrl: v, baseUrlCustom: false })
                  }
                >
                  <FormSelectOption value="" label="Pick a discovered endpoint…" isDisabled />
                  {endpointOptions.map((u) => (
                    <FormSelectOption key={u} value={u} label={u} />
                  ))}
                  <FormSelectOption value="__custom__" label="Custom URL…" />
                </FormSelect>
                {(form.baseUrlCustom || (!!form.baseUrl && !endpointOptions.includes(form.baseUrl))) && (
                  <TextInput
                    style={{ marginTop: 8 }}
                    value={form.baseUrl}
                    onChange={(_e, v) => patch({ baseUrl: v })}
                    placeholder="http://model-desk.model-desk.svc.cluster.local:4000/v1"
                  />
                )}
                <FormHelperText>
                  <HelperText>
                    <HelperTextItem>
                      Discovered from in-cluster model servers and already-published
                      connections. Custom is the escape hatch.
                    </HelperTextItem>
                  </HelperText>
                </FormHelperText>
              </FormGroup>
              <FormGroup label="API dialect">
                <FormSelect value={form.api} onChange={(_e, v) => patch({ api: v })}>
                  {API_DIALECTS.map((d) => (
                    <FormSelectOption key={d} value={d} label={d} />
                  ))}
                </FormSelect>
              </FormGroup>
              <FormGroup label="API key">
                <TextInput
                  type="password"
                  value={form.apiKeyInput}
                  onChange={(_e, v) => patch({ apiKeyInput: v })}
                  placeholder={
                    editing === 'new'
                      ? 'Stored as a Secret in agent-office; never shown again'
                      : `Leave blank to keep the current key (${keyRef().name})`
                  }
                />
                <FormHelperText>
                  <HelperText>
                    <HelperTextItem>
                      Write-only. Saved into Secret {ADMIN_NS}/{keyRef().name}; the operator
                      projects it onto gateways — nothing else can read it from here.
                    </HelperTextItem>
                  </HelperText>
                </FormHelperText>
              </FormGroup>
            </>
          )}
          <FormGroup label="Models" isRequired>
            {form.kind === 'endpoint' && (
              <>
                <Button
                  variant="secondary"
                  icon={probing ? <Spinner size="sm" /> : <SearchIcon />}
                  isDisabled={!canProbe || probing}
                  onClick={fetchModels}
                >
                  Fetch models from the endpoint
                </Button>
                {!canProbe && (
                  <FormHelperText>
                    <HelperText>
                      <HelperTextItem>
                        Needs the endpoint URL and a key (typed above, or already stored).
                      </HelperTextItem>
                    </HelperText>
                  </FormHelperText>
                )}
                {probeError && (
                  <Alert variant="warning" isInline title={probeError} style={{ marginTop: 8 }} />
                )}
                {probed && probed.length > 0 && (
                  <div
                    style={{
                      marginTop: 8,
                      maxHeight: 180,
                      overflowY: 'auto',
                      border: '1px solid var(--pf-v5-global--BorderColor--100)',
                      borderRadius: 4,
                      padding: 8,
                    }}
                  >
                    {probed.map((id) => (
                      <Checkbox
                        key={id}
                        id={`probed-${id}`}
                        label={id}
                        isChecked={form.models.some((m) => m.id === id)}
                        onChange={() => toggleModel(id)}
                      />
                    ))}
                  </div>
                )}
              </>
            )}
            {form.kind !== 'endpoint' && (
              <div>
                {KNOWN_SUBSCRIPTION_MODELS.map((m) => (
                  <Checkbox
                    key={m.id}
                    id={`known-${m.id}`}
                    label={`${m.name} (${m.id})`}
                    isChecked={form.models.some((x) => x.id === m.id)}
                    onChange={() =>
                      setForm((f) => ({
                        ...f,
                        models: f.models.some((x) => x.id === m.id)
                          ? f.models.filter((x) => x.id !== m.id)
                          : [...f.models, m],
                      }))
                    }
                  />
                ))}
              </div>
            )}
            {form.models.length > 0 && (
              <div style={{ marginTop: 12 }}>
                <HelperText>
                  <HelperTextItem>
                    Offered on the picker (first is the default). Labels are optional.
                  </HelperTextItem>
                </HelperText>
                {form.models.map((m) => (
                  <Flex key={m.id} style={{ marginTop: 6 }} alignItems={{ default: 'alignItemsCenter' }}>
                    <FlexItem style={{ minWidth: 180 }}>
                      <code>{m.id}</code>
                    </FlexItem>
                    <FlexItem>
                      <TextInput
                        aria-label={`label for ${m.id}`}
                        value={m.name ?? ''}
                        onChange={(_e, v) => setModelLabel(m.id, v)}
                        placeholder="Friendly label (optional)"
                      />
                    </FlexItem>
                    <FlexItem>
                      <Button
                        variant="plain"
                        aria-label={`remove ${m.id}`}
                        onClick={() => toggleModel(m.id)}
                      >
                        <TimesIcon />
                      </Button>
                    </FlexItem>
                  </Flex>
                ))}
              </div>
            )}
          </FormGroup>
          <FormGroup label="Visible to groups">
            <MultiPicker
              id="groups-picker"
              options={groupOptions}
              value={form.groups}
              onChange={(groups) => patch({ groups })}
              placeholder="Pick groups…"
              addHint="Add SSO-only group"
            />
          </FormGroup>
          <FormGroup label="Visible to users">
            <MultiPicker
              id="users-picker"
              options={userOptions}
              value={form.users}
              onChange={(users) => patch({ users })}
              placeholder="Pick users…"
              addHint="Add SSO-only user"
            />
            <FormHelperText>
              <HelperText>
                <HelperTextItem>
                  Matched against the Developer Hub sign-in (group memberships and
                  username). Names already granted on other connections appear in
                  the lists even when they only exist in SSO.
                </HelperTextItem>
              </HelperText>
            </FormHelperText>
          </FormGroup>
          <FormGroup label="Key strategy">
            <FormSelect value={form.keyStrategy} onChange={(_e, v) => patch({ keyStrategy: v })}>
              <FormSelectOption value="shared" label="shared — one key for every consumer" />
              <FormSelectOption
                value="perSeat"
                label="perSeat — per-consumer budgeted keys (minting lands with seat tooling)"
              />
            </FormSelect>
          </FormGroup>
        </Form>
      </Modal>

      {/* Unpublish */}
      <Modal
        variant={ModalVariant.small}
        title={`Unpublish ${deleting?.spec?.displayName || deleting?.metadata?.name}?`}
        isOpen={deleting !== null}
        onClose={() => setDeleting(null)}
        actions={[
          <Button key="del" variant="danger" onClick={doDelete} isLoading={busy} isDisabled={busy}>
            Unpublish
          </Button>,
          <Button key="cancel" variant="link" onClick={() => setDeleting(null)}>
            Cancel
          </Button>,
        ]}
      >
        <p>
          It disappears from every hiring menu, and the operator removes its
          provider blocks and projected keys from all gateways. Agents already
          pointed at it stop resolving this brain.
        </p>
        {deleting?.spec?.kind === 'endpoint' && deleting?.spec?.apiKeySecretRef?.name && (
          <Checkbox
            id="del-secret"
            style={{ marginTop: 12 }}
            isChecked={deleteSecretToo}
            onChange={(_e, v) => setDeleteSecretToo(v)}
            label={`Also delete key Secret ${ADMIN_NS}/${deleting.spec.apiKeySecretRef.name}`}
          />
        )}
      </Modal>
    </>
  );
};

export default ModelConnectionsPage;
