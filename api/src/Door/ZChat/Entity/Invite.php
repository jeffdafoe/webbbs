<?php

namespace App\Door\ZChat\Entity;

use App\Door\ZChat\Repository\InviteRepository;
use App\Entity\User;
use Doctrine\ORM\Mapping as ORM;
use Symfony\Bridge\Doctrine\Types\UuidType;
use Symfony\Component\Uid\Uuid;

#[ORM\Entity(repositoryClass: InviteRepository::class)]
#[ORM\Table(name: 'zchat_invite')]
#[ORM\Index(columns: ['invited_user_id', 'is_accepted'], name: 'idx_zchat_invite_user')]
class Invite
{
    #[ORM\Id]
    #[ORM\Column(type: UuidType::NAME, unique: true)]
    private Uuid $id;

    #[ORM\ManyToOne]
    #[ORM\JoinColumn(nullable: false)]
    private Room $room;

    #[ORM\ManyToOne]
    #[ORM\JoinColumn(nullable: false)]
    private User $invitedUser;

    #[ORM\ManyToOne]
    #[ORM\JoinColumn(nullable: false)]
    private User $invitedBy;

    #[ORM\Column]
    private \DateTimeImmutable $expiresAt;

    #[ORM\Column(nullable: true)]
    private ?bool $isAccepted = null;

    #[ORM\Column]
    private \DateTimeImmutable $createdAt;

    public function __construct()
    {
        $this->id = Uuid::v7();
        $this->createdAt = new \DateTimeImmutable();
        $this->expiresAt = new \DateTimeImmutable('+1 hour');
    }

    public function getId(): Uuid
    {
        return $this->id;
    }

    public function getRoom(): Room
    {
        return $this->room;
    }

    public function setRoom(Room $room): static
    {
        $this->room = $room;
        return $this;
    }

    public function getInvitedUser(): User
    {
        return $this->invitedUser;
    }

    public function setInvitedUser(User $invitedUser): static
    {
        $this->invitedUser = $invitedUser;
        return $this;
    }

    public function getInvitedBy(): User
    {
        return $this->invitedBy;
    }

    public function setInvitedBy(User $invitedBy): static
    {
        $this->invitedBy = $invitedBy;
        return $this;
    }

    public function getExpiresAt(): \DateTimeImmutable
    {
        return $this->expiresAt;
    }

    public function setExpiresAt(\DateTimeImmutable $expiresAt): static
    {
        $this->expiresAt = $expiresAt;
        return $this;
    }

    public function isAccepted(): ?bool
    {
        return $this->isAccepted;
    }

    public function accept(): static
    {
        $this->isAccepted = true;
        return $this;
    }

    public function decline(): static
    {
        $this->isAccepted = false;
        return $this;
    }

    public function isPending(): bool
    {
        return $this->isAccepted === null;
    }

    public function isExpired(): bool
    {
        return $this->expiresAt < new \DateTimeImmutable();
    }

    public function isValid(): bool
    {
        return $this->isPending() && !$this->isExpired();
    }

    public function getCreatedAt(): \DateTimeImmutable
    {
        return $this->createdAt;
    }
}
