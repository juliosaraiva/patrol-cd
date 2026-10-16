import {
  createContext,
  useContext,
  useEffect,
  useId,
  useRef,
  type ComponentPropsWithRef,
  type ReactNode,
} from 'react';
import { IconChevronDown, IconError } from '../icons';
import styles from './Form.module.css';

interface FieldContextValue {
  id: string;
  describedBy: string | undefined;
  invalid: boolean;
  required: boolean;
}

const FieldContext = createContext<FieldContextValue | null>(null);

export interface FormFieldProps {
  label: ReactNode;
  children: ReactNode;
  help?: ReactNode;
  /** Error message; marks the control aria-invalid and links the message. */
  error?: ReactNode;
  required?: boolean;
  /** Explicit control id (defaults to a generated one). */
  id?: string;
  className?: string;
}

/**
 * Label, help text and error for one control. Input, TextArea and Select rendered inside pick up
 * the id, aria-describedby, aria-invalid and required attributes automatically.
 */
export function FormField({ label, children, help, error, required = false, id, className }: FormFieldProps) {
  const generated = useId();
  const controlId = id ?? `field-${generated}`;
  const helpId = help ? `${controlId}-help` : undefined;
  const errorId = error ? `${controlId}-error` : undefined;
  const describedBy = [errorId, helpId].filter(Boolean).join(' ') || undefined;
  return (
    <FieldContext.Provider value={{ id: controlId, describedBy, invalid: Boolean(error), required }}>
      <div className={[styles.field, className].filter(Boolean).join(' ')}>
        <label className={styles.label} htmlFor={controlId}>
          {label}
          {required ? (
            <span className={styles.required} aria-hidden="true">
              *
            </span>
          ) : null}
        </label>
        {children}
        {error ? (
          <p id={errorId} className={styles.error}>
            <IconError size={14} />
            <span>{error}</span>
          </p>
        ) : null}
        {help ? (
          <p id={helpId} className={styles.help}>
            {help}
          </p>
        ) : null}
      </div>
    </FieldContext.Provider>
  );
}

function useFieldProps(props: { id?: string | undefined; 'aria-describedby'?: string | undefined; 'aria-invalid'?: unknown; required?: boolean | undefined }) {
  const field = useContext(FieldContext);
  if (!field) return {};
  return {
    id: props.id ?? field.id,
    'aria-describedby': [props['aria-describedby'], field.describedBy].filter(Boolean).join(' ') || undefined,
    'aria-invalid': field.invalid || undefined,
    required: props.required ?? (field.required || undefined),
  };
}

function cx(...names: (string | false | undefined)[]): string {
  return names.filter(Boolean).join(' ');
}

export interface InputProps extends Omit<ComponentPropsWithRef<'input'>, 'size'> {
  size?: 'sm' | 'md';
  /** Monospace text for identifiers (URLs, revisions, paths). */
  mono?: boolean;
}

export function Input({ size = 'md', mono = false, className, ...rest }: InputProps) {
  const field = useFieldProps(rest);
  return <input className={cx(styles.control, size === 'sm' && styles.sm, mono && styles.mono, className)} {...rest} {...field} />;
}

export interface TextAreaProps extends ComponentPropsWithRef<'textarea'> {
  mono?: boolean;
}

export function TextArea({ mono = false, className, rows = 4, ...rest }: TextAreaProps) {
  const field = useFieldProps(rest);
  return <textarea rows={rows} className={cx(styles.control, styles.textarea, mono && styles.mono, className)} {...rest} {...field} />;
}

export interface SelectOption {
  value: string;
  label: string;
  disabled?: boolean;
}

export interface SelectProps extends Omit<ComponentPropsWithRef<'select'>, 'size'> {
  options?: readonly SelectOption[];
  size?: 'sm' | 'md';
  placeholder?: string;
}

/** Native <select>: full keyboard and screen-reader support for free. */
export function Select({ options, size = 'md', placeholder, className, children, ...rest }: SelectProps) {
  const field = useFieldProps(rest);
  return (
    <span className={styles.selectWrap}>
      <select className={cx(styles.control, styles.select, size === 'sm' && styles.sm, className)} {...rest} {...field}>
        {placeholder !== undefined ? (
          <option value="" disabled>
            {placeholder}
          </option>
        ) : null}
        {options?.map((o) => (
          <option key={o.value} value={o.value} disabled={o.disabled}>
            {o.label}
          </option>
        ))}
        {children}
      </select>
      <span className={styles.selectIcon} aria-hidden="true">
        <IconChevronDown size={14} />
      </span>
    </span>
  );
}

export interface CheckboxProps extends Omit<ComponentPropsWithRef<'input'>, 'type'> {
  label: ReactNode;
  description?: ReactNode;
  indeterminate?: boolean;
}

export function Checkbox({ label, description, indeterminate = false, className, ref, ...rest }: CheckboxProps) {
  const inner = useRef<HTMLInputElement | null>(null);
  useEffect(() => {
    if (inner.current) inner.current.indeterminate = indeterminate;
  }, [indeterminate]);
  return (
    <label className={cx(styles.check, className)}>
      <input
        type="checkbox"
        ref={(el) => {
          inner.current = el;
          if (typeof ref === 'function') ref(el);
          else if (ref) ref.current = el;
        }}
        aria-checked={indeterminate ? 'mixed' : undefined}
        {...rest}
      />
      <span className={styles.checkText}>
        <span>{label}</span>
        {description ? <span className={styles.help}>{description}</span> : null}
      </span>
    </label>
  );
}

export interface RadioProps extends Omit<ComponentPropsWithRef<'input'>, 'type'> {
  label: ReactNode;
  description?: ReactNode;
}

export function Radio({ label, description, className, ...rest }: RadioProps) {
  return (
    <label className={cx(styles.check, className)}>
      <input type="radio" {...rest} />
      <span className={styles.checkText}>
        <span>{label}</span>
        {description ? <span className={styles.help}>{description}</span> : null}
      </span>
    </label>
  );
}

export interface RadioGroupProps<V extends string> {
  legend: ReactNode;
  name?: string;
  value: V;
  onChange: (value: V) => void;
  options: readonly { value: V; label: ReactNode; description?: ReactNode; disabled?: boolean }[];
  orientation?: 'vertical' | 'horizontal';
  className?: string;
}

/** Native radios in a fieldset: arrow keys move the selection as browsers implement it. */
export function RadioGroup<V extends string>({
  legend,
  name,
  value,
  onChange,
  options,
  orientation = 'vertical',
  className,
}: RadioGroupProps<V>) {
  const generated = useId();
  const groupName = name ?? `radio-${generated}`;
  return (
    <fieldset className={cx(styles.fieldset, className)}>
      <legend className={styles.legend}>{legend}</legend>
      <div className={cx(styles.options, orientation === 'horizontal' && styles.horizontal)}>
        {options.map((o) => (
          <Radio
            key={o.value}
            name={groupName}
            value={o.value}
            checked={o.value === value}
            disabled={o.disabled}
            onChange={() => onChange(o.value)}
            label={o.label}
            description={o.description}
          />
        ))}
      </div>
    </fieldset>
  );
}

export interface SwitchProps extends Omit<ComponentPropsWithRef<'button'>, 'onChange' | 'children'> {
  checked: boolean;
  onChange: (checked: boolean) => void;
  label: ReactNode;
}

/** On/off toggle (role="switch"); Space and Enter toggle it as a native button. */
export function Switch({ checked, onChange, label, className, disabled, ...rest }: SwitchProps) {
  return (
    <button
      type="button"
      role="switch"
      aria-checked={checked}
      className={cx(styles.switch, className)}
      disabled={disabled}
      onClick={() => onChange(!checked)}
      {...rest}
    >
      <span className={styles.track} aria-hidden="true">
        <span className={styles.thumb} />
      </span>
      <span>{label}</span>
    </button>
  );
}
