DELETE FROM public.session;
ALTER TABLE public.session ADD COLUMN created_at TIMESTAMPTZ NOT NULL;
